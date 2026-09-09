package main

import (
	"sort"
	"strings"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// partitionTracker turns disruptoor's applied state into partition records
// with applied/lifted slots; the caller serialises.
type partitionTracker struct {
	d       *disruptoor.Client
	classOf func(name string) string
	anchor  int
	recs    []partition    // oldest first
	open    map[string]int // name -> index into recs while applied
}

func newPartitionTracker(d *disruptoor.Client, classOf func(name string) string, anchor int) *partitionTracker {
	return &partitionTracker{d: d, classOf: classOf, anchor: anchor, open: map[string]int{}}
}

// poll records first sightings at nowSlot and lifts names no longer applied;
// a name re-applied later is a new record. An error leaves the state as is.
func (p *partitionTracker) poll(nowSlot uint64) error {
	applied, err := p.d.Partitions()
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(applied))
	for _, a := range applied {
		seen[a.Name] = true
		if idx, ok := p.open[a.Name]; ok {
			p.recs[idx].Victims = victimsOf(a.Groups, p.anchor)
			continue
		}
		p.recs = append(p.recs, partition{
			Name: a.Name, Class: p.classOf(a.Name), Victims: victimsOf(a.Groups, p.anchor), AppliedSlot: nowSlot,
		})
		p.open[a.Name] = len(p.recs) - 1
	}
	for name, idx := range p.open {
		if !seen[name] {
			p.recs[idx].LiftedSlot = nowSlot
			delete(p.open, name)
		}
	}
	return nil
}

func (p *partitionTracker) list() []partition {
	out := make([]partition, len(p.recs))
	copy(out, p.recs)
	return out
}

// isolated reports whether node is a victim of an applied partition.
func (p *partitionTracker) isolated(node int) bool {
	for _, idx := range p.open {
		if containsInt(p.recs[idx].Victims, node) {
			return true
		}
	}
	return false
}

// covered reports whether slot falls in a partition's window (applied-2 ..
// lifted+grace, open while applied): node 0 (no node on the event) matches
// any partition, a participant must be one of its victims, anything else
// (node < 0, the monitor itself) is never explained by a partition.
func (p *partitionTracker) covered(slot uint64, node int, graceSlots uint64) bool {
	if node < 0 {
		return false
	}
	for _, rec := range p.recs {
		if node != 0 && !containsInt(rec.Victims, node) {
			continue
		}
		if slot+2 < rec.AppliedSlot {
			continue
		}
		if rec.LiftedSlot == 0 {
			return true
		}
		if slot <= rec.LiftedSlot+graceSlots {
			return true
		}
	}
	return false
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// victimsOf names the isolated side(s): every node outside the group holding
// the anchor; without the anchor, outside the largest group (first on a tie).
// A lone group can only be the isolated side. Sorted ascending.
func victimsOf(groups [][]int, anchor int) []int {
	if len(groups) == 0 {
		return nil
	}
	if len(groups) == 1 {
		out := append([]int(nil), groups[0]...)
		sort.Ints(out)
		return out
	}
	network := -1
	for i, g := range groups {
		if containsInt(g, anchor) {
			network = i
			break
		}
	}
	if network < 0 {
		network = 0
		for i, g := range groups {
			if len(g) > len(groups[network]) {
				network = i
			}
		}
	}
	var out []int
	for i, g := range groups {
		if i != network {
			out = append(out, g...)
		}
	}
	sort.Ints(out)
	return out
}

// classifier maps a partition name to its class: the schedule's ops by name,
// the gate's post-op by its -short-/-deep- infix, proposer-* isolations as
// short, anything else (pbt-* scenarios) as scenario.
func classifier(s *migsched.Schedule) func(name string) string {
	known := map[string]string{}
	if s != nil {
		for _, op := range s.Ops {
			known[op.Name] = string(op.Class)
		}
	}
	return func(name string) string {
		if c, ok := known[name]; ok {
			return c
		}
		if strings.HasPrefix(name, "proposer-") || strings.Contains(name, "-short-") {
			return "short"
		}
		if strings.Contains(name, "-deep-") {
			return "deep"
		}
		return "scenario"
	}
}

// scheduleView renders the ops in slots.
func scheduleView(s *migsched.Schedule, genesis, slotSeconds uint64) []scheduleOp {
	out := make([]scheduleOp, len(s.Ops))
	for i, op := range s.Ops {
		out[i] = scheduleOp{
			Name: op.Name, Class: string(op.Class), Victims: append([]int(nil), op.Victims...),
			StartSlot: slotOf(genesis, slotSeconds, uint64(op.Start.Unix())),
			EndSlot:   slotOf(genesis, slotSeconds, uint64(op.End.Unix())),
		}
	}
	return out
}

// dirPhase: the binary direction's phase before the fork.
func dirPhase(d *migmon.DirectionProgress) string {
	if d.Error != "" || d.Phase == migmon.DirStalled {
		return "stalled"
	}
	switch d.Phase {
	case migmon.DirFollowing:
		return "following"
	case migmon.DirSynced:
		return "synced"
	case migmon.DirParked, migmon.DirIdle: // idle: no handle yet, nothing runs
		return "parked"
	default:
		return "unknown"
	}
}

// merklePhase: the merkle direction's phase after the fork.
func merklePhase(d *migmon.DirectionProgress) string {
	if d.Error != "" || d.Phase == migmon.DirStalled {
		return "stalled"
	}
	if migmon.Active(d) {
		return "window"
	}
	if d.Phase == migmon.DirParked || d.Phase == migmon.DirIdle {
		return "parked"
	}
	return "unknown"
}

// nodePhase maps a client's progress to the page vocabulary: done is done;
// before the fork the binary direction decides, after it the merkle one (or
// the chain phase without one). The cursor follows the deciding direction.
func nodePhase(p *migmon.MigrationProgress, forkTime, headTime uint64) (phase string, cursor uint64, cursorHash string) {
	if p == nil {
		return "unknown", 0, ""
	}
	if p.Phase == migmon.PhaseDone {
		return "done", 0, ""
	}
	if headTime < forkTime {
		if p.Binary == nil {
			return "unknown", 0, ""
		}
		return dirPhase(p.Binary), uint64(p.Binary.Cursor), p.Binary.CursorHash
	}
	if p.Merkle != nil {
		return merklePhase(p.Merkle), uint64(p.Merkle.Cursor), p.Merkle.CursorHash
	}
	switch p.Phase {
	case migmon.PhaseRunning:
		phase = "window"
	case migmon.PhaseDone:
		phase = "done"
	default:
		phase = "unknown"
	}
	if p.Binary != nil {
		return phase, uint64(p.Binary.Cursor), p.Binary.CursorHash
	}
	return phase, 0, ""
}
