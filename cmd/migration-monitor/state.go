package main

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// maxRows bounds the sampled-height and event rings the live view keeps.
// Twenty rows is what fits on a screen an operator reads from across a
// desk; the JSONL stream is the full record, this is only the window.
const maxRows = 20

// Root-matrix verdicts. A row's verdict is what an operator reads first, so
// the vocabulary separates the two harmless cases - no record written yet,
// and a canonical-chain split during a partition - from a real
// disagreement, which is the only one that means the run is broken.
const (
	verdictEqual       = "all-equal"
	verdictDiffer      = "differ"
	verdictNotRecorded = "not-yet-recorded"
	verdictPartial     = "partial"
	verdictSplit       = "canonical-split"
)

// Shadow-root cell states. A bare empty string collapses four different
// facts into one blank cell, which is how an operator comes to read a
// healthy run as broken; each case names itself instead.
const (
	rootValue       = "value"            // a root was recorded, Root holds it
	rootNull        = "not-recorded"     // introspection answered: no record written yet, which is legal
	rootNoSurface   = "no-introspection" // this client exposes no shadow-root surface at all
	rootUnavailable = "unavailable"      // the fetch for this node failed on this tick
)

// snapshot is the live view's state: what the poll and sample paths have
// most recently observed, in the shape the page and /api/state render.
//
// One mutex guards it, held only for the field copies. Every caller is a
// short critical section and the HTTP handlers work on a copy, so a slow
// client can never stall the poll loop.
type snapshot struct {
	mu       sync.Mutex
	forkTime uint64

	order   []string // node names in --el order, so matrix columns stay put
	nodes   map[string]nodeView
	samples []sampleRow
	events  []eventRow

	pending []byte // partial JSONL line carried between Write calls
}

// apiState is the /api/state document and the page's template input. Field
// names are the machine surface a lap driver tails, so they are stable and
// say what they hold.
type apiState struct {
	Now           string      `json:"now"`
	ForkTime      uint64      `json:"fork_time"`
	SecondsToFork int64       `json:"seconds_to_fork"` // negative once the fork time has passed
	Nodes         []nodeView  `json:"nodes"`
	Samples       []sampleRow `json:"samples"`
	Events        []eventRow  `json:"events"`
}

type nodeView struct {
	Name          string `json:"name"`
	Client        string `json:"client"`
	Introspection bool   `json:"introspection"` // false: watched over standard RPC only
	Down          bool   `json:"down"`
	HaveHead      bool   `json:"have_head"`
	Head          uint64 `json:"head"`
	HaveProgress  bool   `json:"have_progress"`
	Phase         string `json:"phase"`
	// Binary and Merkle are nil when the node's progress report carried no
	// such direction, which is not the same as a direction sitting idle.
	Binary    *directionView `json:"binary"`
	Merkle    *directionView `json:"merkle"`
	ForkBlock *forkBlockView `json:"fork_block"` // nil until the fork block is observed
}

type directionView struct {
	Phase      string `json:"phase"`
	Cursor     uint64 `json:"cursor"`
	CursorHash string `json:"cursor_hash"`
	ShadowRoot string `json:"shadow_root"`
	Error      string `json:"error"`
}

type forkBlockView struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
	Final  bool   `json:"final"` // false: provisional, a reorg can still orphan it
}

type sampleRow struct {
	Time    string     `json:"time"`
	Height  uint64     `json:"height"`
	Verdict string     `json:"verdict"`
	Roots   []rootCell `json:"roots"`
}

type rootCell struct {
	Node   string `json:"node"`
	Root   string `json:"root"`
	Status string `json:"status"`
}

type eventRow struct {
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Node    string `json:"node"`
	Number  uint64 `json:"number"`
	Finding string `json:"finding"`
	Detail  string `json:"detail"`
}

func newSnapshot(forkTime uint64) *snapshot {
	return &snapshot{forkTime: forkTime, nodes: map[string]nodeView{}}
}

// setNode records one node's poll result. The poll loop calls it after
// every tick, including a tick that failed, so an unreachable node shows as
// down rather than freezing at its last good reading.
func (s *snapshot) setNode(ns *nodeState) {
	v := nodeView{
		Name:          ns.name,
		Client:        migmon.ClientType(ns.name),
		Introspection: ns.introspection,
		Down:          ns.down,
		HaveHead:      ns.haveHead,
		Head:          ns.lastHead,
		HaveProgress:  ns.introspection && ns.haveProgress,
	}
	if v.HaveProgress {
		v.Phase = ns.lastProgress.Phase
		v.Binary = newDirectionView(ns.lastProgress.Binary)
		v.Merkle = newDirectionView(ns.lastProgress.Merkle)
	}
	if b := ns.timeline.BStarRecord(); b != nil {
		v.ForkBlock = &forkBlockView{Number: b.Number, Hash: b.Hash, Final: ns.timeline.BStarIsFinal()}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.nodes[ns.name]; !seen {
		s.order = append(s.order, ns.name)
	}
	s.nodes[ns.name] = v
}

func newDirectionView(d *migmon.DirectionProgress) *directionView {
	if d == nil {
		return nil
	}
	return &directionView{
		Phase:      d.Phase,
		Cursor:     uint64(d.Cursor),
		CursorHash: d.CursorHash,
		ShadowRoot: d.ShadowRoot,
		Error:      d.Error,
	}
}

// addSample records one cross-node shadow-root sample as a matrix row. It
// takes every node, not only the ones that answered, so the matrix keeps a
// fixed column per node and a missing answer reads as a failed fetch
// instead of vanishing.
func (s *snapshot) addSample(now time.Time, height uint64, states []*nodeState, samples []migmon.NodeSample, split bool) {
	answered := make(map[string]migmon.NodeSample, len(samples))
	for _, sm := range samples {
		answered[sm.Node] = sm
	}
	cells := make([]rootCell, 0, len(states))
	for _, ns := range states {
		c := rootCell{Node: ns.name}
		sm, ok := answered[ns.name]
		switch {
		case !ok:
			c.Status = rootUnavailable
		case !ns.introspection:
			c.Status = rootNoSurface
		case sm.Root == "":
			c.Status = rootNull
		default:
			c.Status, c.Root = rootValue, sm.Root
		}
		cells = append(cells, c)
	}
	row := sampleRow{
		Time:    now.UTC().Format(time.RFC3339),
		Height:  height,
		Verdict: verdict(cells, split),
		Roots:   cells,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = appendRing(s.samples, row)
}

// verdict judges one matrix row. Cells with no shadow-root surface and
// cells whose fetch failed carry no opinion either way: counting them as
// disagreement would report a fault the chain does not have.
func verdict(cells []rootCell, split bool) string {
	// A canonical-chain split explains differing roots by itself - the
	// nodes are reading different blocks - so it outranks the comparison.
	if split {
		return verdictSplit
	}
	recorded, missing, first, differ := 0, 0, "", false
	for _, c := range cells {
		switch c.Status {
		case rootNull:
			missing++
		case rootValue:
			if recorded == 0 {
				first = c.Root
			} else if c.Root != first {
				differ = true
			}
			recorded++
		}
	}
	switch {
	case differ:
		return verdictDiffer
	case recorded == 0:
		return verdictNotRecorded
	case missing > 0:
		return verdictPartial
	default:
		return verdictEqual
	}
}

// notableEvent reports whether an event kind belongs in the view's event
// list. Progress, head and sample events arrive every tick and would bury
// the ones an operator is watching for.
func notableEvent(kind string) bool {
	switch kind {
	case migmon.EvReorg, migmon.EvBStar, migmon.EvBStarReorged, migmon.EvBStarFinal,
		migmon.EvCritical, migmon.EvWarn:
		return true
	}
	return false
}

// Write feeds the event list from the JSONL stream the monitor already
// writes: every finding in this process routes through one encoder, so
// tee-ing that writer catches all of them - including the batches
// internal/migmon returns from its Observe calls - instead of asking each
// emit site to remember the page too.
//
// It never reports an error: this is a view, and the JSONL stream remains
// the record of anything the display drops.
func (s *snapshot) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		var ev migmon.Event
		if err := json.Unmarshal(line, &ev); err != nil || !notableEvent(ev.Kind) {
			continue
		}
		s.events = appendRing(s.events, eventRow{
			Time:    ev.Time.UTC().Format(time.RFC3339),
			Kind:    ev.Kind,
			Node:    ev.Node,
			Number:  ev.Number,
			Finding: ev.Finding,
			Detail:  ev.Detail,
		})
	}
}

// appendRing appends one row, dropping the oldest once the ring is full.
func appendRing[T any](rows []T, row T) []T {
	rows = append(rows, row)
	if len(rows) > maxRows {
		rows = rows[len(rows)-maxRows:]
	}
	return rows
}

// view copies the state for one reader. Everything a handler needs is
// copied here so the lock is released before any JSON encoding or template
// execution, which is what keeps a slow HTTP client off the poll loop.
func (s *snapshot) view(now time.Time) apiState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := apiState{
		Now:           now.UTC().Format(time.RFC3339),
		ForkTime:      s.forkTime,
		SecondsToFork: int64(s.forkTime) - now.Unix(),
		Nodes:         make([]nodeView, 0, len(s.order)),
		Samples:       append([]sampleRow(nil), s.samples...),
		Events:        append([]eventRow(nil), s.events...),
	}
	for _, name := range s.order {
		st.Nodes = append(st.Nodes, s.nodes[name])
	}
	// The direction and fork-block pointers are built fresh by setNode and
	// never mutated afterwards, so sharing them with a reader is safe.
	return st
}
