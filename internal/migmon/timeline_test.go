package migmon

import (
	"strings"
	"testing"
	"time"
)

func hasFinding(evs []Event, kind, finding string) bool {
	for _, e := range evs {
		if e.Kind == kind && e.Finding == finding {
			return true
		}
	}
	return false
}

func countKind(evs []Event, kind string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestObservePollWarnsOnceOnInitialInactive(t *testing.T) {
	tl := NewTimeline("nodeA", 1000)
	evs := tl.ObservePoll(MigrationProgress{Phase: PhaseInactive}, 1)
	if !hasFinding(evs, EvWarn, "") || countKind(evs, EvWarn) != 1 {
		t.Fatalf("first inactive poll: want one warn, got %+v", evs)
	}
	evs = tl.ObservePoll(MigrationProgress{Phase: PhaseInactive}, 2)
	if countKind(evs, EvWarn) != 0 {
		t.Fatalf("second inactive poll: want no repeat warn, got %+v", evs)
	}
}

// F2 immediate truth table: stalled or errored directions fire once, clear,
// and re-arm.
func TestF2Immediate(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *DirectionProgress
		want bool
	}{
		{"nil direction", nil, false},
		{"idle", &DirectionProgress{Phase: DirIdle}, false},
		{"following healthy", &DirectionProgress{Phase: DirFollowing}, false},
		{"stalled", &DirectionProgress{Phase: DirStalled}, true},
		{"error set", &DirectionProgress{Phase: DirFollowing, Error: "boom"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s dirState
			evs := s.observe("n", "binary", tc.d, 0)
			got := hasFinding(evs, EvCritical, FindingStall)
			if got != tc.want {
				t.Fatalf("got critical=%v, want %v (evs=%+v)", got, tc.want, evs)
			}
		})
	}
}

func TestF2ImmediateDedupAndRearm(t *testing.T) {
	var s dirState
	stalled := &DirectionProgress{Phase: DirStalled}
	healthy := &DirectionProgress{Phase: DirFollowing}

	evs := s.observe("n", "binary", stalled, 0)
	if !hasFinding(evs, EvCritical, FindingStall) {
		t.Fatalf("first stall: want critical, got %+v", evs)
	}
	evs = s.observe("n", "binary", stalled, 0)
	if countKind(evs, EvCritical) != 0 {
		t.Fatalf("persisting stall: want no repeat, got %+v", evs)
	}
	evs = s.observe("n", "binary", healthy, 1)
	if countKind(evs, EvCritical) != 0 {
		t.Fatalf("clearing stall: want no critical, got %+v", evs)
	}
	evs = s.observe("n", "binary", stalled, 2)
	if !hasFinding(evs, EvCritical, FindingStall) {
		t.Fatalf("re-stalling after clear: want critical again, got %+v", evs)
	}
}

// F2 slow burn: cursor frozen for StallPolls consecutive polls while the
// head advances StallHeadDelta blocks over that span.
func TestF2SlowBurn(t *testing.T) {
	run := func(phase string, cursor uint64, headStart uint64, headStep uint64) []Event {
		var s dirState
		var all []Event
		head := headStart
		for i := 0; i < StallPolls+2; i++ {
			d := &DirectionProgress{Phase: phase, Cursor: FlexUint64(cursor)}
			all = append(all, s.observe("n", "binary", d, head)...)
			head += headStep
		}
		return all
	}

	t.Run("frozen cursor and advancing head fires once", func(t *testing.T) {
		evs := run(DirFollowing, 5, 0, StallHeadDelta) // head advances by StallHeadDelta every poll
		if countKind(evs, EvCritical) != 1 {
			t.Fatalf("want exactly one critical, got %+v", evs)
		}
	})
	t.Run("frozen cursor but head does not advance suspends nothing", func(t *testing.T) {
		evs := run(DirFollowing, 5, 0, 0) // head never advances: the head-delta gate can never be satisfied
		if countKind(evs, EvCritical) != 0 {
			t.Fatalf("want no critical when head never advances, got %+v", evs)
		}
	})
	t.Run("idle suspends the stall clock", func(t *testing.T) {
		evs := run(DirIdle, 5, 0, StallHeadDelta)
		if countKind(evs, EvCritical) != 0 {
			t.Fatalf("idle direction must never fire F2 slow, got %+v", evs)
		}
	})
	t.Run("parked suspends the stall clock", func(t *testing.T) {
		evs := run(DirParked, 5, 0, StallHeadDelta)
		if countKind(evs, EvCritical) != 0 {
			t.Fatalf("parked direction must never fire F2 slow (cursor is required to freeze there), got %+v", evs)
		}
	})
	t.Run("moving cursor never fires", func(t *testing.T) {
		var s dirState
		var all []Event
		for i := 0; i < StallPolls+2; i++ {
			d := &DirectionProgress{Phase: DirFollowing, Cursor: FlexUint64(uint64(i))}
			all = append(all, s.observe("n", "binary", d, uint64(i)*StallHeadDelta)...)
		}
		if countKind(all, EvCritical) != 0 {
			t.Fatalf("advancing cursor must never fire F2 slow, got %+v", all)
		}
	})
}

// F3 boundary: compliant within the window fires nothing; never complying
// past both clocks fires exactly one critical.
func TestF3Boundary(t *testing.T) {
	t.Run("compliant immediately", func(t *testing.T) {
		tl := NewTimeline("n", 1000)
		tl.ObserveBStar(BStar{Number: 100, Hash: "0xb"})
		evs := tl.ObservePoll(MigrationProgress{
			Phase:  PhaseRunning,
			Binary: &DirectionProgress{Phase: DirParked},
			Merkle: &DirectionProgress{Phase: DirFollowing},
		}, 101)
		if hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("compliant boundary must not fire, got %+v", evs)
		}
	})
	t.Run("never complies, fires once after both clocks expire", func(t *testing.T) {
		tl := NewTimeline("n", 1000)
		tl.ObserveBStar(BStar{Number: 100, Hash: "0xb"})
		stillMigrating := MigrationProgress{
			Phase:  PhaseRunning,
			Binary: &DirectionProgress{Phase: DirFollowing},
			Merkle: nil,
		}
		var all []Event
		heads := []uint64{101, 102, 104} // BoundaryPolls=2, BoundaryBlocks=3
		for _, h := range heads {
			all = append(all, tl.ObservePoll(stillMigrating, h)...)
		}
		if countKind(all, EvCritical) != 1 || !hasFinding(all, EvCritical, FindingBoundary) {
			t.Fatalf("want exactly one boundary critical, got %+v", all)
		}
		// Further polls must not repeat it.
		more := tl.ObservePoll(stillMigrating, 105)
		if countKind(more, EvCritical) != 0 {
			t.Fatalf("boundary finding must not repeat, got %+v", more)
		}
	})
}

func TestF3DoneEarly(t *testing.T) {
	t.Run("done with no b* observed", func(t *testing.T) {
		tl := NewTimeline("n", 1000)
		evs := tl.ObservePoll(MigrationProgress{Phase: PhaseDone}, 5)
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("done before any b* must be critical F3, got %+v", evs)
		}
	})
	t.Run("done at or before b* is early", func(t *testing.T) {
		tl := NewTimeline("n", 1000)
		tl.ObserveBStar(BStar{Number: 100, Hash: "0xb"})
		evs := tl.ObservePoll(MigrationProgress{Phase: PhaseDone}, 100)
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("done at b* height must be critical F3, got %+v", evs)
		}
	})
	t.Run("done strictly after b* is legal", func(t *testing.T) {
		tl := NewTimeline("n", 1000)
		tl.ObserveBStar(BStar{Number: 100, Hash: "0xb"})
		evs := tl.ObservePoll(MigrationProgress{Phase: PhaseDone}, 101)
		if hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("done after b* must not fire done-early, got %+v", evs)
		}
	})
}

func TestBStarObservedOnce(t *testing.T) {
	tl := NewTimeline("n", 1000)
	evs := tl.ObserveBStar(BStar{Number: 100, Hash: "0xa"})
	if len(evs) != 1 || evs[0].Kind != EvBStar {
		t.Fatalf("want one bstar event, got %+v", evs)
	}
	evs = tl.ObserveBStar(BStar{Number: 200, Hash: "0xz"})
	if len(evs) != 0 || tl.BStarRecord().Number != 100 {
		t.Fatalf("b* must be recorded once; got %+v, record %+v", evs, tl.BStarRecord())
	}
}

// Cross-node fork-block checks: agreement is silent, a bad record shape is
// critical at once, provisional disagreement is legal until it outlives any
// partition, and finalized disagreement is critical immediately.
func TestBStarQuorum(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	good := func(n uint64, hash string) BStar {
		return BStar{Number: n, Hash: hash, Time: 1000, ParentTime: 990}
	}

	t.Run("agreement is silent", func(t *testing.T) {
		q := NewBStarQuorum(999)
		if evs := q.Observe("a", good(100, "0xa"), now); len(evs) != 0 {
			t.Fatalf("first node must not fire, got %+v", evs)
		}
		if evs := q.Observe("b", good(100, "0xa"), now); len(evs) != 0 {
			t.Fatalf("agreeing nodes must not fire, got %+v", evs)
		}
	})

	t.Run("provisional disagreement waits out the grace", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Observe("a", good(100, "0xa"), now)
		// A partition spanning the activation puts each side on its own
		// fork block: legal while it lasts.
		if evs := q.Observe("b", good(105, "0xb"), now); len(evs) != 0 {
			t.Fatalf("fresh disagreement must not fire, got %+v", evs)
		}
		mid := now.Add(BStarProvisionalGrace / 2 * time.Second)
		if evs := q.Observe("b", good(105, "0xb"), mid); len(evs) != 0 {
			t.Fatalf("disagreement inside the grace must not fire, got %+v", evs)
		}
		late := now.Add((BStarProvisionalGrace + 30) * time.Second)
		evs := q.Observe("b", good(105, "0xb"), late)
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("disagreement outliving the grace must fire, got %+v", evs)
		}
		if !strings.Contains(evs[0].Detail, "still disagree") {
			t.Fatalf("evidence %q does not name the persistence", evs[0].Detail)
		}
	})

	t.Run("agreement after a reorg clears the timer", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Observe("a", good(100, "0xa"), now)
		q.Observe("b", good(105, "0xb"), now)
		// The losing branch is reorged away and both land on the same
		// fork block: the earlier disagreement must not be held against
		// them later.
		q.Observe("b", good(100, "0xa"), now.Add(60*time.Second))
		late := now.Add((BStarProvisionalGrace + 60) * time.Second)
		if evs := q.Observe("a", good(100, "0xa"), late); len(evs) != 0 {
			t.Fatalf("converged nodes must not fire later, got %+v", evs)
		}
	})
	t.Run("disagreement fires again after an intervening agreement", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Observe("a", good(100, "0xa"), now)
		q.Observe("b", good(105, "0xb"), now)
		late1 := now.Add((BStarProvisionalGrace + 30) * time.Second)
		if evs := q.Observe("b", good(105, "0xb"), late1); !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("first disagreement outliving the grace must fire, got %+v", evs)
		}
		// The branches converge: disagreeFired must reset alongside
		// disagreeSince, or a second genuine disagreement can never fire.
		agreedAt := late1.Add(time.Second)
		if evs := q.Observe("b", good(100, "0xa"), agreedAt); len(evs) != 0 {
			t.Fatalf("agreement must not fire, got %+v", evs)
		}
		q.Observe("b", good(110, "0xc"), agreedAt.Add(time.Second))
		late2 := agreedAt.Add((BStarProvisionalGrace + 30) * time.Second)
		evs := q.Observe("b", good(110, "0xc"), late2)
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("second disagreement outliving the grace must fire again, got %+v", evs)
		}
	})

	t.Run("finalized disagreement fires at once", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Observe("a", good(100, "0xa"), now)
		q.Observe("b", good(105, "0xb"), now)
		q.Finalize("a", good(100, "0xa"))
		evs := q.Finalize("b", good(105, "0xb"))
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("finalized disagreement must fire immediately, got %+v", evs)
		}
		if !strings.Contains(evs[0].Detail, "finalized fork blocks differ") {
			t.Fatalf("evidence %q does not name finality", evs[0].Detail)
		}
	})

	t.Run("record shape is critical at once", func(t *testing.T) {
		q := NewBStarQuorum(999)
		// time < T: this node did not find the boundary at all.
		evs := q.Observe("a", BStar{Number: 100, Hash: "0xa", Time: 990, ParentTime: 980}, now)
		if !hasFinding(evs, EvCritical, FindingBoundary) || !strings.Contains(evs[0].Detail, "does not straddle") {
			t.Fatalf("want a shape critical, got %+v", evs)
		}
	})
	t.Run("reorged-in record shape is re-validated", func(t *testing.T) {
		q := NewBStarQuorum(999)
		// A reorg replaces node a's provisional record with a new hash;
		// the latch must key on record identity, not node, or a bad
		// replacement's shape is never checked.
		if evs := q.Observe("a", good(100, "0xa"), now); len(evs) != 0 {
			t.Fatalf("valid initial record must not fire, got %+v", evs)
		}
		evs := q.Observe("a", BStar{Number: 101, Hash: "0xb", Time: 990, ParentTime: 980}, now)
		if !hasFinding(evs, EvCritical, FindingBoundary) || !strings.Contains(evs[0].Detail, "does not straddle") {
			t.Fatalf("reorged-in bad-shape record must fire, got %+v", evs)
		}
	})
}

// A reorg that orphans the recorded fork block must re-arm the boundary
// check rather than leave the node pinned to a block nobody has.
func TestBStarReorged(t *testing.T) {
	tl := NewTimeline("n", 1000)
	tl.ObserveBStar(BStar{Number: 100, Hash: "0xold", Time: 1000, ParentTime: 990})
	evs := tl.BStarReorged("0xnew")
	if len(evs) != 1 || evs[0].Kind != EvBStarReorged {
		t.Fatalf("want one bstar-reorged event, got %+v", evs)
	}
	if tl.BStarRecord() != nil {
		t.Fatal("the orphaned record survived; the node would keep judging a boundary nobody has")
	}
	if evs := tl.ObserveBStar(BStar{Number: 103, Hash: "0xnew", Time: 1000, ParentTime: 990}); len(evs) != 1 {
		t.Fatalf("the node could not record its new fork block, got %+v", evs)
	}
	// Once finalized, a reorg claim is refused: finality means it cannot move.
	tl.BStarFinalized()
	if evs := tl.BStarReorged("0xlater"); len(evs) != 0 {
		t.Fatalf("a finalized fork block accepted a reorg, got %+v", evs)
	}
}
