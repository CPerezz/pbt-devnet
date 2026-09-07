package migmon

import (
	"fmt"
	"time"
)

// IStar is one node's record of I*: the first canonical block whose header
// timestamp is >= T. ParentTime lets the quorum check prove the boundary straddles T.
type IStar struct {
	Number     uint64
	Hash       string
	Time       uint64
	ParentTime uint64
}

// Timeline holds one node's migration-phase state machine across polls and
// turns observations into findings; pure, caller supplies decoded results in poll order.
type Timeline struct {
	node string
	t    uint64 // binary trie activation time (unix seconds)

	polls int
	istar *IStar
	// istarFinal: fork block has finalized; a reorg can no longer move it.
	istarFinal bool
	// istarPolls counts ObservePoll calls since I* was recorded.
	istarPolls  int
	boundaryOK  bool
	boundaryHit bool
	doneEarly   bool

	binary dirState
	merkle dirState
}

// dirState is the per-direction stall bookkeeping.
type dirState struct {
	immediateFired bool
	// Slow-burn span: cursor value, consecutive polls held, head at span start.
	spanning  bool
	cursor    uint64
	frozen    int
	spanHead  uint64
	slowFired bool
}

func NewTimeline(node string, binaryTrieTime uint64) *Timeline {
	return &Timeline{node: node, t: binaryTrieTime}
}

// IStarRecord returns the recorded I*, or nil before one is observed.
func (tl *Timeline) IStarRecord() *IStar { return tl.istar }

// ObserveIStar records the node's fork block and emits its event. A record already held is kept.
func (tl *Timeline) ObserveIStar(b IStar) []Event {
	if tl.istar != nil {
		return nil
	}
	tl.istar = &b
	tl.istarPolls = 0
	return []Event{{Kind: EvIStar, Node: tl.node, Number: b.Number, Hash: b.Hash}}
}

// IStarReorged drops a recorded fork block a reorg orphaned and re-arms the
// boundary check against the new canonical chain; provisional until finalized.
func (tl *Timeline) IStarReorged(nowCanonical string) []Event {
	if tl.istar == nil || tl.istarFinal {
		return nil
	}
	old := *tl.istar
	tl.istar = nil
	tl.istarPolls = 0
	tl.boundaryOK = false
	tl.boundaryHit = false
	return []Event{{
		Kind: EvIStarReorged, Node: tl.node, Number: old.Number, Hash: nowCanonical,
		Detail: fmt.Sprintf("fork block %d %s was orphaned; height %d now holds %s",
			old.Number, old.Hash, old.Number, nowCanonical),
	}}
}

// IStarFinalized marks the recorded fork block as settled, once.
func (tl *Timeline) IStarFinalized() []Event {
	if tl.istar == nil || tl.istarFinal {
		return nil
	}
	tl.istarFinal = true
	return []Event{{
		Kind: EvIStarFinal, Node: tl.node, Number: tl.istar.Number, Hash: tl.istar.Hash,
		Detail: "fork block finalized",
	}}
}

// IStarIsFinal reports whether the fork block has finalized.
func (tl *Timeline) IStarIsFinal() bool { return tl.istarFinal }

// ObservePoll ingests one poll's decoded progress and head. Call ObserveIStar
// first on the tick that crosses T so the "done strictly after I*" check sees the boundary.
func (tl *Timeline) ObservePoll(p MigrationProgress, head uint64) []Event {
	var evs []Event

	// An inactive first phase means the devnet is not migration-armed.
	if tl.polls == 0 && p.Phase == PhaseInactive {
		evs = append(evs, Event{
			Kind: EvWarn, Node: tl.node, Phase: p.Phase,
			Detail: "initial migration phase is inactive; devnet is not migration-armed",
		})
	}
	tl.polls++

	evs = append(evs, tl.binary.observe(tl.node, "binary", p.Binary, head)...)
	evs = append(evs, tl.merkle.observe(tl.node, "merkle", p.Merkle, head)...)

	// After I*, binary must park and merkle must start following (or be
	// synced) within BoundaryPolls polls or BoundaryBlocks blocks.
	if tl.istar != nil && !tl.boundaryOK && !tl.boundaryHit {
		tl.istarPolls++
		binParked := p.Binary != nil && p.Binary.Phase == DirParked
		if binParked && Active(p.Merkle) {
			tl.boundaryOK = true
		} else if tl.istarPolls > BoundaryPolls && head > tl.istar.Number+BoundaryBlocks {
			tl.boundaryHit = true
			evs = append(evs, Event{
				Kind: EvCritical, Node: tl.node, Finding: FindingBoundary, Number: tl.istar.Number,
				Detail: fmt.Sprintf("boundary window expired %d polls and %d blocks after I* %d: binary %s, merkle %s",
					tl.istarPolls, head-tl.istar.Number, tl.istar.Number, dirPhase(p.Binary), dirPhase(p.Merkle)),
			})
		}
	}

	// "done" is only legal strictly after I*.
	if p.Phase == PhaseDone && !tl.doneEarly && (tl.istar == nil || head <= tl.istar.Number) {
		tl.doneEarly = true
		detail := "phase done before any I* was observed"
		if tl.istar != nil {
			detail = fmt.Sprintf("phase done at head %d, not strictly after I* %d", head, tl.istar.Number)
		}
		evs = append(evs, Event{Kind: EvCritical, Node: tl.node, Finding: FindingBoundary, Detail: detail})
	}

	return evs
}

func dirPhase(d *DirectionProgress) string {
	if d == nil {
		return "nil"
	}
	return d.Phase
}

// observe runs both stall forms for one direction.
func (s *dirState) observe(node, dir string, d *DirectionProgress, head uint64) []Event {
	var evs []Event

	// Latched until the condition clears so a persisting stall reads as one finding.
	if d != nil && (d.Phase == DirStalled || d.Error != "") {
		if !s.immediateFired {
			s.immediateFired = true
			evs = append(evs, Event{
				Kind: EvCritical, Node: node, Finding: FindingStall,
				Detail: fmt.Sprintf("%s direction phase %s error %q", dir, d.Phase, d.Error),
			})
		}
	} else {
		s.immediateFired = false
	}

	// Cursor frozen while the head moves; suspended while idle/absent or
	// parked (parked is required to freeze at I*).
	if d == nil || d.Phase == DirIdle || d.Phase == DirParked {
		s.spanning = false
		return evs
	}
	cur := uint64(d.Cursor)
	if !s.spanning || cur != s.cursor {
		s.spanning = true
		s.cursor = cur
		s.frozen = 1
		s.spanHead = head
		s.slowFired = false
		return evs
	}
	s.frozen++
	if !s.slowFired && s.frozen >= StallPolls && head >= s.spanHead+StallHeadDelta {
		s.slowFired = true
		evs = append(evs, Event{
			Kind: EvCritical, Node: node, Finding: FindingStall, Number: cur,
			Detail: fmt.Sprintf("%s cursor frozen at %d for %d polls while head advanced %d blocks", dir, cur, s.frozen, head-s.spanHead),
		})
	}
	return evs
}

// IStarQuorum cross-checks fork-block records across nodes. A record's own
// shape (header time at/past activation, parent below it) is critical
// immediately. Agreement between nodes is provisional until the fork block
// finalizes; finalized disagreement is critical immediately.
type IStarQuorum struct {
	T uint64

	provisional map[string]IStar
	final       map[string]IStar
	shaped      map[string]bool

	disagreeSince time.Time
	disagreeFired bool
}

func NewIStarQuorum(t uint64) *IStarQuorum {
	return &IStarQuorum{
		T:           t,
		provisional: make(map[string]IStar),
		final:       make(map[string]IStar),
		shaped:      make(map[string]bool),
	}
}

// Observe records a node's current fork block, replacing any earlier one.
func (q *IStarQuorum) Observe(node string, b IStar, now time.Time) []Event {
	q.provisional[node] = b
	evs := q.checkShape(node, b)
	return append(evs, q.checkProvisional(now)...)
}

// Finalize records that a node's fork block has settled.
func (q *IStarQuorum) Finalize(node string, b IStar) []Event {
	if _, ok := q.final[node]; ok {
		return nil
	}
	q.final[node] = b
	var evs []Event
	for other, rec := range q.final {
		if other == node || rec.Number == b.Number && rec.Hash == b.Hash {
			continue
		}
		evs = append(evs, Event{
			Kind: EvCritical, Node: node, Finding: FindingBoundary, Number: b.Number,
			Detail: fmt.Sprintf("finalized fork blocks differ: %s has %d (%s), %s has %d (%s)",
				node, b.Number, b.Hash, other, rec.Number, rec.Hash),
		})
	}
	return evs
}

// checkShape validates one record against the activation time, once per
// record identity (a reorg can replace a node's provisional record).
func (q *IStarQuorum) checkShape(node string, b IStar) []Event {
	key := node + "/" + b.Hash
	if q.shaped[key] {
		return nil
	}
	q.shaped[key] = true
	if b.Time >= q.T && b.ParentTime < q.T {
		return nil
	}
	return []Event{{
		Kind: EvCritical, Node: node, Finding: FindingBoundary, Number: b.Number,
		Detail: fmt.Sprintf("fork block %d does not straddle the activation %d (time %d, parent %d)",
			b.Number, q.T, b.Time, b.ParentTime),
	}}
}

// checkProvisional fires once nodes disagree longer than any partition could explain.
func (q *IStarQuorum) checkProvisional(now time.Time) []Event {
	if len(q.provisional) < 2 {
		return nil
	}
	var ref *IStar
	var refNode string
	agreed := true
	for node, rec := range q.provisional {
		if ref == nil {
			r := rec
			ref, refNode = &r, node
			continue
		}
		if rec.Number != ref.Number || rec.Hash != ref.Hash {
			agreed = false
			if q.disagreeSince.IsZero() {
				q.disagreeSince = now
			}
			if !q.disagreeFired && now.Sub(q.disagreeSince) > IStarProvisionalGrace*time.Second {
				q.disagreeFired = true
				return []Event{{
					Kind: EvCritical, Node: node, Finding: FindingBoundary, Number: rec.Number,
					Detail: fmt.Sprintf("fork blocks still disagree after %.0fs, longer than any partition holds: %s has %d (%s), %s has %d (%s)",
						now.Sub(q.disagreeSince).Seconds(), node, rec.Number, rec.Hash, refNode, ref.Number, ref.Hash),
				}}
			}
			break
		}
	}
	if agreed {
		// Re-arm so a later, unrelated disagreement can fire on its own.
		q.disagreeSince = time.Time{}
		q.disagreeFired = false
	}
	return nil
}
