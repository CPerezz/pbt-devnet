package migmon

import "fmt"

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

// ObserveBStar records the node's b* once and emits the bstar event.
// Repeats are ignored: b* is a fact, not a stream.
func (tl *Timeline) ObserveBStar(b BStar) []Event {
	if tl.bstar != nil {
		return nil
	}
	tl.bstar = &b
	tl.bstarPolls = 0
	return []Event{{Kind: EvBStar, Node: tl.node, Number: b.Number, Hash: b.Hash}}
}

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

// BStarQuorum cross-checks b* records across nodes. Once two nodes have one,
// their numbers must agree and each record's header times must straddle T
// (time >= T, parent < T); anything else is a boundary the nodes do not agree
// on, which is critical F3.
type BStarQuorum struct {
	T         uint64
	ref       string
	seen      map[string]BStar
	straddled map[string]bool
}

func NewBStarQuorum(t uint64) *BStarQuorum {
	return &BStarQuorum{T: t, seen: make(map[string]BStar), straddled: make(map[string]bool)}
}

// Add records one node's b* and returns any cross-check findings. Each node
// is straddle-checked exactly once, after quorum (>= 2 records) is reached.
func (q *BStarQuorum) Add(node string, b BStar) []Event {
	if _, ok := q.seen[node]; ok {
		return nil
	}
	q.seen[node] = b
	if len(q.seen) == 1 {
		q.ref = node
		return nil
	}

	var evs []Event
	if ref := q.seen[q.ref]; b.Number != ref.Number {
		evs = append(evs, Event{
			Kind: EvCritical, Node: node, Finding: FindingBoundary, Number: b.Number,
			Detail: fmt.Sprintf("bstar-disagreement: %s says b*=%d (%s), %s says b*=%d (%s)",
				node, b.Number, b.Hash, q.ref, ref.Number, ref.Hash),
		})
	}
	for n, rec := range q.seen {
		if q.straddled[n] {
			continue
		}
		q.straddled[n] = true
		if rec.Time >= q.T && rec.ParentTime < q.T {
			continue
		}
		evs = append(evs, Event{
			Kind: EvCritical, Node: n, Finding: FindingBoundary, Number: rec.Number,
			Detail: fmt.Sprintf("bstar-disagreement: %s b* %d does not straddle T=%d (time %d, parent %d)",
				n, rec.Number, q.T, rec.Time, rec.ParentTime),
		})
	}
	return evs
}
