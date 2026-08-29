package migmon

import (
	"fmt"
	"time"
)

// BStar is one node's record of b*: the first canonical block whose header
// timestamp is >= T. ParentTime is carried so the quorum check can prove the
// boundary really straddles T rather than trusting the walk that found it.
type BStar struct {
	Number     uint64
	Hash       string
	Time       uint64
	ParentTime uint64
}

// Timeline holds one node's migration-phase state machine across polls and
// turns observations into findings. It is pure: the caller does all RPC and
// feeds decoded results in poll order.
type Timeline struct {
	node string
	t    uint64 // binary trie activation time (unix seconds)

	polls int
	bstar *BStar
	// bstarFinal records that the fork block has finalized, after which a
	// reorg can no longer move it.
	bstarFinal bool
	// bstarPolls counts ObservePoll calls since b* was recorded; the F3
	// window is measured in these plus blocks past b*.
	bstarPolls  int
	boundaryOK  bool
	boundaryHit bool
	doneEarly   bool

	binary dirState
	merkle dirState
}

// dirState is the per-direction F2 bookkeeping.
type dirState struct {
	immediateFired bool
	// Slow-burn span: cursor value, how many consecutive polls it has held,
	// and the head when the span started.
	spanning  bool
	cursor    uint64
	frozen    int
	spanHead  uint64
	slowFired bool
}

func NewTimeline(node string, binaryTrieTime uint64) *Timeline {
	return &Timeline{node: node, t: binaryTrieTime}
}

// BStarRecord returns the recorded b*, or nil before one is observed.
func (tl *Timeline) BStarRecord() *BStar { return tl.bstar }

// ObserveBStar records the node's fork block and emits its event. A record
// already held is kept: use BStarReorged first if a reorg orphaned it.
func (tl *Timeline) ObserveBStar(b BStar) []Event {
	if tl.bstar != nil {
		return nil
	}
	tl.bstar = &b
	tl.bstarPolls = 0
	return []Event{{Kind: EvBStar, Node: tl.node, Number: b.Number, Hash: b.Hash}}
}

// BStarReorged drops a recorded fork block that a reorg has orphaned and
// re-arms the boundary check against the new canonical chain. Until the
// fork block finalizes it is provisional: a partition spanning the
// activation has each side cross it on its own block, and the branch that
// loses takes its boundary with it.
func (tl *Timeline) BStarReorged(nowCanonical string) []Event {
	if tl.bstar == nil || tl.bstarFinal {
		return nil
	}
	old := *tl.bstar
	tl.bstar = nil
	tl.bstarPolls = 0
	tl.boundaryOK = false
	tl.boundaryHit = false
	return []Event{{
		Kind: EvBStarReorged, Node: tl.node, Number: old.Number, Hash: nowCanonical,
		Detail: fmt.Sprintf("fork block %d %s was orphaned; height %d now holds %s",
			old.Number, old.Hash, old.Number, nowCanonical),
	}}
}

// BStarFinalized marks the recorded fork block as settled, once.
func (tl *Timeline) BStarFinalized() []Event {
	if tl.bstar == nil || tl.bstarFinal {
		return nil
	}
	tl.bstarFinal = true
	return []Event{{
		Kind: EvBStarFinal, Node: tl.node, Number: tl.bstar.Number, Hash: tl.bstar.Hash,
		Detail: "fork block finalized",
	}}
}

// BStarIsFinal reports whether the fork block has finalized.
func (tl *Timeline) BStarIsFinal() bool { return tl.bstarFinal }

// ObservePoll ingests one poll's decoded progress and head, returning any
// findings it triggers. Call ObserveBStar first on the tick that crosses T so
// the "done strictly after b*" check sees the boundary.
func (tl *Timeline) ObservePoll(p MigrationProgress, head uint64) []Event {
	var evs []Event

	// A migration devnet whose nodes come up inactive was mis-armed; say so
	// once at startup rather than silently watching nothing happen.
	if tl.polls == 0 && p.Phase == PhaseInactive {
		evs = append(evs, Event{
			Kind: EvWarn, Node: tl.node, Phase: p.Phase,
			Detail: "initial migration phase is inactive; devnet is not migration-armed",
		})
	}
	tl.polls++

	evs = append(evs, tl.binary.observe(tl.node, "binary", p.Binary, head)...)
	evs = append(evs, tl.merkle.observe(tl.node, "merkle", p.Merkle, head)...)

	// F3 boundary window: after b*, the binary direction must park and the
	// merkle direction must start following (or already be synced) within
	// BoundaryPolls polls or BoundaryBlocks blocks — the violation fires
	// only once BOTH clocks have expired, since either window satisfies the
	// contract's "or".
	if tl.bstar != nil && !tl.boundaryOK && !tl.boundaryHit {
		tl.bstarPolls++
		binParked := p.Binary != nil && p.Binary.Phase == DirParked
		if binParked && Active(p.Merkle) {
			tl.boundaryOK = true
		} else if tl.bstarPolls > BoundaryPolls && head > tl.bstar.Number+BoundaryBlocks {
			tl.boundaryHit = true
			evs = append(evs, Event{
				Kind: EvCritical, Node: tl.node, Finding: FindingBoundary, Number: tl.bstar.Number,
				Detail: fmt.Sprintf("boundary window expired %d polls and %d blocks after b* %d: binary %s, merkle %s",
					tl.bstarPolls, head-tl.bstar.Number, tl.bstar.Number, dirPhase(p.Binary), dirPhase(p.Merkle)),
			})
		}
	}

	// F3 done-too-early: "done" is only legal strictly after b*. Seeing it
	// with no b* recorded, or while the head has not passed b*, means the
	// node ended the migration before the fork boundary existed.
	if p.Phase == PhaseDone && !tl.doneEarly && (tl.bstar == nil || head <= tl.bstar.Number) {
		tl.doneEarly = true
		detail := "phase done before any b* was observed"
		if tl.bstar != nil {
			detail = fmt.Sprintf("phase done at head %d, not strictly after b* %d", head, tl.bstar.Number)
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

// observe runs both F2 forms for one direction.
func (s *dirState) observe(node, dir string, d *DirectionProgress, head uint64) []Event {
	var evs []Event

	// F2 immediate: the follower says it is stalled, or carries an error.
	// Latched until the condition clears so a stall that persists for a
	// hundred polls reads as one finding, not a hundred.
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

	// F2 slow burn: cursor frozen while the head moves. Suspended while the
	// direction is idle or absent (nothing is supposed to move) and while
	// parked (the cursor is REQUIRED to freeze at b*, per F3); a frozen
	// cursor only indicts a direction that claims to be working.
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

// BStarQuorum cross-checks fork-block records across nodes. Two things are
// checked, and they are not equally urgent.
//
// A record's own shape is structural: its header time must be at or past
// the activation and its parent's below it, or the node did not find the
// boundary at all. That is critical the moment it is seen.
//
// Agreement BETWEEN nodes is provisional until the fork block finalizes. A
// partition spanning the activation puts each side on its own fork block by
// design - that is the behaviour under test, not a fault - so provisional
// disagreement is only critical once it outlives any window a schedule
// holds. Finalized disagreement is critical immediately: finality means the
// losing branch cannot come back, so two nodes that finalized different
// fork blocks have permanently different chains.
type BStarQuorum struct {
	T uint64

	provisional map[string]BStar
	final       map[string]BStar
	shaped      map[string]bool

	disagreeSince time.Time
	disagreeFired bool
}

func NewBStarQuorum(t uint64) *BStarQuorum {
	return &BStarQuorum{
		T:           t,
		provisional: make(map[string]BStar),
		final:       make(map[string]BStar),
		shaped:      make(map[string]bool),
	}
}

// Observe records a node's current fork block, replacing any earlier one -
// a reorg can move it - and returns findings.
func (q *BStarQuorum) Observe(node string, b BStar, now time.Time) []Event {
	q.provisional[node] = b
	evs := q.checkShape(node, b)
	return append(evs, q.checkProvisional(now)...)
}

// Finalize records that a node's fork block has settled.
func (q *BStarQuorum) Finalize(node string, b BStar) []Event {
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

// checkShape validates one record against the activation time, once.
func (q *BStarQuorum) checkShape(node string, b BStar) []Event {
	if q.shaped[node] {
		return nil
	}
	q.shaped[node] = true
	if b.Time >= q.T && b.ParentTime < q.T {
		return nil
	}
	return []Event{{
		Kind: EvCritical, Node: node, Finding: FindingBoundary, Number: b.Number,
		Detail: fmt.Sprintf("fork block %d does not straddle the activation %d (time %d, parent %d)",
			b.Number, q.T, b.Time, b.ParentTime),
	}}
}

// checkProvisional times how long the nodes have disagreed and fires once
// the disagreement has lasted longer than any partition could explain.
func (q *BStarQuorum) checkProvisional(now time.Time) []Event {
	if len(q.provisional) < 2 {
		return nil
	}
	var ref *BStar
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
			if !q.disagreeFired && now.Sub(q.disagreeSince) > BStarProvisionalGrace*time.Second {
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
		q.disagreeSince = time.Time{}
	}
	return nil
}
