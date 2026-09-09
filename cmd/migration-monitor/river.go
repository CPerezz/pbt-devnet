package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// collector assembles the page's document once per tick from the nodes' heads
// (walked back to known blocks), shadow roots per block, disruptoor's applied
// partitions, peer counts and finality, and publishes it as one JSON blob. Only
// the poll goroutine touches its state; the JSONL stream is teed into alerts.
type collector struct {
	genesis, slotSeconds, forkTime, windowSlots uint64
	anchor                                      int
	states                                      []*nodeState
	beacons                                     map[int]string // node -> beacon API base

	lin    *lineage
	shadow *shadowTable
	parts  *partitionTracker
	sched  []scheduleOp

	seq       uint64
	elPeers   map[int]int
	clPeers   map[int]int
	finalized map[int]uint64 // slot of each node's finalized block
	alerts    []alert
	partsDown bool

	alertsMu sync.Mutex // Write runs inside Log.Emit, which the tick calls: its own lock
	pending  []alert
	doc      atomic.Pointer[[]byte] // the last published document
}

// Bounds: parent walk per head, shadow asks per node per tick, retention (a
// 12h window of 6s slots), per-node RPC budget per tick, alerts kept.
const (
	walkDepth   = 512
	shadowBatch = 8
	retainSlots = 7200
	nodeTimeout = 8 * time.Second
	maxAlerts   = 500
)

func newCollector(states []*nodeState, beacons map[int]string, d *disruptoor.Client, sched []scheduleOp, classOf func(string) string,
	anchor int, genesis, slotSeconds, forkTime, windowSlots uint64) *collector {
	c := &collector{
		genesis: genesis, slotSeconds: slotSeconds, forkTime: forkTime, windowSlots: windowSlots, anchor: anchor,
		states: states, beacons: beacons, sched: sched,
		elPeers: map[int]int{}, clPeers: map[int]int{}, finalized: map[int]uint64{},
	}
	c.lin = newLineage(forkTime, c.slotOf, retainSlots, anchor)
	clients := make([]migmon.Client, len(states))
	for i, ns := range states {
		clients[i] = ns.rpc
	}
	c.shadow = newShadowTable(clients, 10*time.Second, retainSlots)
	if d != nil {
		c.parts = newPartitionTracker(d, classOf, anchor)
	}
	return c
}

func (c *collector) slotOf(unix uint64) uint64 { return slotOf(c.genesis, c.slotSeconds, unix) }

func slotOf(genesis, slotSeconds, unix uint64) uint64 {
	if unix < genesis {
		return 0
	}
	return (unix - genesis) / slotSeconds
}

// tick advances every feed once and publishes the document. Each node gets
// its own deadline so one stalled RPC cannot hold the page dark; pollOnce
// already reports reachability, so failures here are silent.
func (c *collector) tick(ctx context.Context, log *migmon.Log) {
	for i, ns := range c.states {
		node := i + 1
		nctx, cancel := context.WithTimeout(ctx, nodeTimeout)
		c.pollNode(nctx, log, node, ns)
		cancel()
	}
	now := c.slotOf(uint64(time.Now().Unix()))
	c.lin.settle(now)
	if c.parts != nil {
		err := c.parts.poll(now)
		if err != nil && !c.partsDown {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor", Detail: "disruptoor state: " + err.Error()})
		}
		c.partsDown = err != nil
	}
	sctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	c.shadow.step(sctx, log, time.Now(), now, shadowBatch)
	cancel()
	c.lin.prune(now)
	c.judgeAlerts()
	c.seq++
	if raw, err := json.Marshal(c.state(now)); err == nil {
		c.doc.Store(&raw)
	} else {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor", Detail: "encoding the page document: " + err.Error()})
	}
}

func (c *collector) pollNode(ctx context.Context, log *migmon.Log, node int, ns *nodeState) {
	head, err := ns.rpc.HeaderByTag(ctx, "latest")
	if err != nil || head == nil {
		return
	}
	got, err := c.lin.extend(ctx, ns.rpc, head, walkDepth)
	if err != nil {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: "walking the chain behind the head: " + err.Error()})
	}
	for _, h := range got {
		c.shadow.track(h.Hash, c.slotOf(h.Time))
	}
	c.lin.observe(node, head.Hash)
	if n, err := ns.rpc.PeerCount(ctx); err == nil {
		c.elPeers[node] = n
	}
	if url := c.beacons[node]; url != "" {
		if n, err := migmon.BeaconPeerCount(ctx, url); err == nil {
			c.clPeers[node] = n
		}
	}
	if fin, err := ns.rpc.HeaderByTag(ctx, "finalized"); err == nil && fin != nil {
		c.finalized[node] = c.slotOf(fin.Time)
	}
}

// document returns the last published JSON, nil before the first tick.
func (c *collector) document() []byte {
	if p := c.doc.Load(); p != nil {
		return *p
	}
	return nil
}

// Write tees the JSONL stream: warnings and criticals become alerts, judged
// against the partitions on the next tick.
func (c *collector) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(p, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev migmon.Event
		if err := json.Unmarshal(line, &ev); err != nil || (ev.Kind != migmon.EvWarn && ev.Kind != migmon.EvCritical) {
			continue
		}
		a := alert{Slot: c.slotOf(uint64(ev.Time.Unix())), Kind: ev.Kind, Node: nodeIndex(ev.Node), Detail: ev.Detail}
		c.alertsMu.Lock()
		c.pending = append(c.pending, a)
		c.alertsMu.Unlock()
	}
	return len(p), nil
}

// judgeAlerts moves teed alerts into the document, marking expected the ones
// that fell inside a partition applied at the time.
func (c *collector) judgeAlerts() {
	c.alertsMu.Lock()
	batch := c.pending
	c.pending = nil
	c.alertsMu.Unlock()
	for _, a := range batch {
		if c.parts != nil {
			a.Expected = c.parts.covered(a.Slot, a.Node, migmon.ConvergenceGrace/c.slotSeconds+2)
		}
		c.alerts = append(c.alerts, a)
	}
	if len(c.alerts) > maxAlerts {
		c.alerts = c.alerts[len(c.alerts)-maxAlerts:]
	}
}

// nodeIndex reads the participant number out of a service name (el-<n>-... or
// cl-<n>-...); 0 for an event with no node, -1 for another source such as
// "monitor" - whose warnings no partition explains away.
func nodeIndex(service string) int {
	if service == "" {
		return 0
	}
	parts := strings.Split(service, "-")
	if len(parts) < 2 || (parts[0] != "el" && parts[0] != "cl") {
		return -1
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return n
}

// state assembles the document. Agreement is judged per block against the
// live holders of its segment that still run a shadow (introspect, not done).
func (c *collector) state(now uint64) apiState {
	segs := c.lin.segments()
	orphaned := map[string]bool{}
	for _, s := range segs {
		orphaned[s.ID] = s.State == "orphaned"
	}
	nodes := make([]nodeView, 0, len(c.states))
	for i, ns := range c.states {
		node := i + 1
		v := nodeView{ID: node, Name: ns.name, Phase: "unknown", IStar: "none", CLPeers: -1, Status: "ok"}
		if head, ok := c.lin.byHash[c.lin.heads[node]]; ok {
			v.Head, v.HeadNumber, v.HeadSlot = head.Hash, head.Number, c.slotOf(head.Time)
			v.Segment = c.lin.segmentOf(head.Hash)
			var prog *migmon.MigrationProgress
			if ns.haveProgress {
				p := ns.lastProgress
				prog = &p
			}
			var cursor uint64
			v.Phase, cursor, v.CursorHash = nodePhase(prog, c.forkTime, head.Time)
			if v.CursorHash != "" {
				v.CursorNumber = cursor
				if cursor < head.Number {
					v.Lag = head.Number - cursor
				}
				v.CursorDetached = orphaned[c.lin.segmentOf(v.CursorHash)]
			}
		}
		if ns.timeline.IStarRecord() != nil {
			v.IStar = "provisional"
			if ns.timeline.IStarIsFinal() {
				v.IStar = "final"
			}
		}
		v.ELPeers = c.elPeers[node]
		if n, ok := c.clPeers[node]; ok {
			v.CLPeers = n
		}
		v.FinalizedSlot = c.finalized[node]
		if ns.down {
			v.Status = "unreachable"
		}
		if c.parts != nil {
			v.Isolated = c.parts.isolated(node)
		}
		nodes = append(nodes, v)
	}

	from := uint64(0)
	if now > c.windowSlots {
		from = now - c.windowSlots
	}
	blocks := c.lin.blocks(from, now)
	for i := range blocks {
		b := &blocks[i]
		b.ShadowRoots = c.shadow.roots(b.Hash)
		holders := c.lin.holders(b.Segment)
		var expected []int
		for _, n := range holders {
			if c.states[n-1].rpc.Introspects() && nodes[n-1].Phase != "done" {
				expected = append(expected, n)
			}
		}
		b.Agreement, b.Dissent = agreement(b.ShadowRoots, holders, expected, orphaned[b.Segment])
		b.Dissent = nonNil(b.Dissent)
	}

	// Finality as the anchor sees it; the lowest view when it is silent.
	finalized, ok := c.finalized[c.anchor]
	if !ok {
		for _, f := range c.finalized {
			if !ok || f < finalized {
				finalized, ok = f, true
			}
		}
	}
	var parts []partition
	if c.parts != nil {
		parts = c.parts.list()
	}
	alerts := append([]alert(nil), c.alerts...)
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].Slot < alerts[j].Slot })
	return apiState{
		Seq: c.seq, NowSlot: now, SlotSeconds: c.slotSeconds, ForkSlot: c.slotOf(c.forkTime), FinalizedSlot: finalized,
		Truncated: c.lin.truncated,
		Nodes:     nodes, Segments: nonNil(segs), Blocks: nonNil(blocks), Reorgs: nonNil(c.lin.reorgs()),
		Partitions: nonNil(parts), Schedule: nonNil(c.sched), Alerts: nonNil(alerts),
	}
}

// nonNil keeps empty lists as [] on the wire; the page indexes them without checks.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// beacons maps --cl name=url entries to participant numbers.
func beacons(log *slog.Logger, cls []string) map[int]string {
	out := map[int]string{}
	for _, cl := range cls {
		name, url, ok := strings.Cut(cl, "=")
		if n := nodeIndex(name); !ok || n <= 0 || url == "" {
			cli.Fatal(log, "want --cl cl-<n>-...=url, got %q", cl)
		}
		out[nodeIndex(name)] = url
	}
	return out
}

// resolveSchedule renders the reorg service's profile for the ribbon. The
// page observes; a profile that does not resolve is a warning, not an exit.
func resolveSchedule(jsonl *migmon.Log, profile string, anchor int, anchorShare float64, protect []int, participants int,
	genesis, fork int64, slotSeconds uint64) ([]scheduleOp, func(string) string) {
	if profile == "" || profile == "none" {
		return nil, classifier(nil)
	}
	s, err := migsched.Resolve(profile, time.Unix(genesis, 0), time.Unix(fork, 0), migsched.Topology{
		Anchor: anchor, Lights: migsched.Lights(participants, anchor, protect), Participants: participants,
		AnchorShare: anchorShare, SecondsPerSlot: int(slotSeconds),
	})
	if err != nil {
		jsonl.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor", Detail: "schedule ribbon disabled: " + err.Error()})
		return nil, classifier(nil)
	}
	return scheduleView(&s, uint64(genesis), slotSeconds), classifier(&s)
}
