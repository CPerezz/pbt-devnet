package migmon

import (
	"strings"
	"testing"
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

// F3 cross-node: agreement is silent, a number disagreement and a straddle
// failure are each their own critical.
func TestBStarQuorum(t *testing.T) {
	t.Run("agreement is silent", func(t *testing.T) {
		q := NewBStarQuorum(999)
		if evs := q.Add("a", BStar{Number: 100, Hash: "0xa", Time: 1000, ParentTime: 990}); len(evs) != 0 {
			t.Fatalf("first node must not fire, got %+v", evs)
		}
		evs := q.Add("b", BStar{Number: 100, Hash: "0xa", Time: 1000, ParentTime: 990})
		if len(evs) != 0 {
			t.Fatalf("agreeing straddled b* must not fire, got %+v", evs)
		}
	})
	t.Run("number disagreement", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Add("a", BStar{Number: 100, Hash: "0xa", Time: 1000, ParentTime: 990})
		evs := q.Add("b", BStar{Number: 105, Hash: "0xb", Time: 1000, ParentTime: 990})
		if !hasFinding(evs, EvCritical, FindingBoundary) || !strings.Contains(evs[0].Detail, "bstar-disagreement") {
			t.Fatalf("want bstar-disagreement critical, got %+v", evs)
		}
	})
	t.Run("straddle failure", func(t *testing.T) {
		q := NewBStarQuorum(999)
		q.Add("a", BStar{Number: 100, Hash: "0xa", Time: 1000, ParentTime: 990})
		evs := q.Add("b", BStar{Number: 100, Hash: "0xa", Time: 990, ParentTime: 980}) // time < T
		if !hasFinding(evs, EvCritical, FindingBoundary) {
			t.Fatalf("want straddle-failure critical, got %+v", evs)
		}
		found := false
		for _, e := range evs {
			if e.Node == "b" && strings.Contains(e.Detail, "does not straddle") {
				found = true
			}
		}
		if !found {
			t.Fatalf("want straddle failure attributed to node b, got %+v", evs)
		}
	})
}
