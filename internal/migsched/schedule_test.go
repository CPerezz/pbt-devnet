package migsched

import (
	"testing"
	"time"
)

// A four-node topology: participant 1 is the bootnode (never disruptable,
// so absent here), 2 holds 40% of the validators, 3 and 4 the rest.
// topo is the shipped layout: the bootnode anchors 40%, three lights hold 20% each.
func topo() Topology {
	return Topology{Anchor: 1, Lights: []int{2, 3, 4}, Participants: 4, AnchorShare: 0.40, SecondsPerSlot: 6}
}

func victimsEqual(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func at(base time.Time, secs int) time.Time { return base.Add(time.Duration(secs) * time.Second) }

func sweepsEqual(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("failsafes = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("failsafe %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// preForkFixture is a pre-fork-only profile for exercising rotation,
// refusal and sweep placement: three deeps over rotating pairs of lights,
// then two shorts rotating over single lights. Injected because no shipped profile is
// pre-fork-only anymore, and these behaviors must not depend on which
// profiles happen to ship.
func preForkFixture(t *testing.T) string {
	t.Helper()
	const name = "test-prefork"
	profiles[name] = profile{
		windows: []window{
			{start: 240, end: 430, anchor: FromGenesis, class: ClassDeep},
			{start: 540, end: 730, anchor: FromGenesis, class: ClassDeep},
			{start: 840, end: 1030, anchor: FromGenesis, class: ClassDeep},
			{start: 1090, end: 1240, anchor: FromGenesis, class: ClassShort},
			{start: 1300, end: 1450, anchor: FromGenesis, class: ClassShort},
		},
		preForkMargin: 300,
	}
	t.Cleanup(func() { delete(profiles, name) })
	return name
}

// The pre-fork shape: deeps cycle through pairs of lights, shorts rotate over
// single lights, everything is admitted with room to spare.
func TestResolvePreFork(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	fork := at(genesis, 1800)
	s, err := Resolve(preForkFixture(t), genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start, end int
		victims    []int
		class      Class
	}{
		{240, 430, []int{2, 3}, ClassDeep},
		{540, 730, []int{3, 4}, ClassDeep},
		{840, 1030, []int{4, 2}, ClassDeep},
		{1090, 1240, []int{2}, ClassShort},
		{1300, 1450, []int{3}, ClassShort},
	}
	if len(s.Ops) != len(want) {
		t.Fatalf("ops = %d, want %d", len(s.Ops), len(want))
	}
	for i, w := range want {
		o := s.Ops[i]
		if !o.Admitted() {
			t.Fatalf("op %d refused: %s", i, o.Refused)
		}
		if !o.Start.Equal(at(genesis, w.start)) || !o.End.Equal(at(genesis, w.end)) {
			t.Fatalf("op %d = [%s,%s], want [+%ds,+%ds]", i, o.Start, o.End, w.start, w.end)
		}
		if o.Class != w.class || !victimsEqual(o.Victims, w.victims) || o.Mutual {
			t.Fatalf("op %d = %s on %v (mutual=%v), want %s on %v as one island", i, o.Class, o.Victims, o.Mutual, w.class, w.victims)
		}
	}
	// Two pre-fork sweeps: the last admitted end, then the deadline, which
	// fires whatever happened before it.
	sweepsEqual(t, s.Failsafes, []time.Time{at(genesis, 1450), at(genesis, 1500)})
	if !s.Quiet.Equal(at(genesis, 1500)) {
		t.Fatalf("quiet = %s, want fork-300 = +1500", s.Quiet)
	}
	if _, ok := s.Straddle(); ok {
		t.Fatal("the pre-fork profile resolved a straddle")
	}
}

// A fork too close for some windows: the crossing ops are refused whole,
// never compressed, and the early sweep tracks the last ADMITTED end.
func TestPreForkAdmission(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	fork := at(genesis, 1200) // deadline +900
	s, err := Resolve(preForkFixture(t), genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range s.Ops[:2] {
		if !o.Admitted() {
			t.Fatalf("op %d ends before the deadline but was refused: %s", i, o.Refused)
		}
	}
	for i, o := range s.Ops[2:] {
		if o.Admitted() {
			t.Fatalf("op %d ends %s, past the +900 deadline, but was admitted", i+2, o.End)
		}
	}
	sweepsEqual(t, s.Failsafes, []time.Time{at(genesis, 730), at(genesis, 900)})
	// A refused op must never contribute a legal-divergence window: it
	// does not run, so divergence in its slot is a fault.
	if s.Covers(at(genesis, 900)) {
		t.Fatal("a refused op still marks its window as legal chaos")
	}
}

// The composite profile carries the straddle anchored to the fork, adds the
// post-fork window op, leaves the restart gap alone, and sweeps five times:
// the last pre-fork end, the pre-fork deadline, the straddle's latest heal,
// its repeat a minute later, and the window op's repeat.
func TestResolveComposite(t *testing.T) {
	genesis := time.Unix(2_000_000, 0)
	fork := at(genesis, 1800)
	s, err := Resolve("composite", genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Ops) != 5 {
		t.Fatalf("ops = %d, want 5", len(s.Ops))
	}
	for _, o := range s.Ops {
		if !o.Admitted() {
			t.Fatalf("op %s refused: %s", o.Name, o.Refused)
		}
	}
	str, ok := s.Straddle()
	if !ok {
		t.Fatal("composite resolved without a straddle")
	}
	if !str.Start.Equal(at(fork, -120)) || !str.End.Equal(at(fork, 60)) || !str.HoldUntil.Equal(at(fork, 180)) {
		t.Fatalf("straddle = [%s,%s] hold %s, want fork-120..fork+60, hold to fork+180", str.Start, str.End, str.HoldUntil)
	}
	// Every light on its own island: each crosses the fork on its own block
	// and every one of them rewinds onto the anchor's chain at the heal.
	if !str.Mutual || !victimsEqual(str.Victims, []int{2, 3, 4}) {
		t.Fatalf("straddle victims = %v mutual=%v, want every light, mutually isolated", str.Victims, str.Mutual)
	}
	// The window op lives inside the open migration window, on a single light.
	var win *Op
	for i := range s.Ops {
		if s.Ops[i].Class == ClassWindow {
			win = &s.Ops[i]
		}
	}
	if win == nil {
		t.Fatal("composite resolved without a window op")
	}
	if !win.Start.Equal(at(fork, 300)) || !win.End.Equal(at(fork, 450)) {
		t.Fatalf("window op = [%s,%s], want fork+300..fork+450", win.Start, win.End)
	}
	if len(win.Victims) != 1 || win.Victims[0] == topo().Anchor {
		t.Fatalf("window victims = %v, want one light", win.Victims)
	}
	// The restart gap: no partition may be open while a node is being
	// restarted there.
	holeFrom, holeTo := at(genesis, 1000), at(genesis, 1300)
	for _, o := range s.Ops {
		if o.Start.Before(holeTo) && o.End.After(holeFrom) {
			t.Fatalf("op %s [%s,%s] intrudes on the restart gap", o.Name, o.Start, o.End)
		}
	}
	sweepsEqual(t, s.Failsafes, []time.Time{
		at(genesis, 940), at(genesis, 1410), at(fork, 180), at(fork, 240), at(fork, 510),
	})
	if !s.Quiet.Equal(at(fork, 570)) {
		t.Fatalf("quiet = %s, want fork+570 (window op end + quiet tail)", s.Quiet)
	}
}

// The straddle's own divergence must read as LEGAL for its whole window
// plus the heal: a watchdog treating sustained divergence as a fault would
// otherwise cut the straddle short and destroy the only evidence this
// profile exists to produce.
func TestStraddleDivergenceIsLegal(t *testing.T) {
	genesis := time.Unix(3_000_000, 0)
	fork := at(genesis, 1800)
	s, err := Resolve("composite", genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []int{-119, -30, 0, 30, 60, 150, 180, 299, 310, 440, 560} {
		if !s.Covers(at(fork, probe)) {
			t.Fatalf("fork%+ds reads as illegal divergence; a watchdog would cut the op short", probe)
		}
	}
	// Past the window op's heal allowance it must stop being legal, or a
	// genuinely wedged node would never be noticed. The straddle's own
	// tail ends at fork+300 (latest heal + tail), exactly where the window
	// op opens; fork+600 is past everything.
	for _, probe := range []int{600, 900} {
		if s.Covers(at(fork, probe)) {
			t.Fatalf("fork+%ds still reads as legal chaos; a wedge would go unhealed", probe)
		}
	}
	// The restart gap is quiet: the last pre-fork op ended at +940 and its
	// allowance runs to +1060.
	for _, probe := range []int{1100, 1300} {
		if s.Covers(at(genesis, probe)) {
			t.Fatalf("genesis+%ds reads as legal divergence, but no op is open then", probe)
		}
	}
}

// Straddle geometry is a resolve-time error, never a silent refusal: a
// straddle profile that cannot straddle would otherwise pass by omission.
// The cases are injected as profiles because the shipped ones are correct
// by construction - this guards future edits to that table.
func TestStraddleGeometryErrors(t *testing.T) {
	genesis := time.Unix(4_000_000, 0)
	fork := at(genesis, 900)

	for name, w := range map[string][]window{
		"too-little-before": {{start: -30, end: 60, anchor: FromFork, class: ClassStraddle}},
		"too-little-after":  {{start: -90, end: 30, anchor: FromFork, class: ClassStraddle}},
		// end+hold must stay inside the bound: 90+120 > 180
		"too-long-after": {{start: -90, end: 90, anchor: FromFork, class: ClassStraddle}},
		"two-straddles": {
			{start: -90, end: 60, anchor: FromFork, class: ClassStraddle},
			{start: 300, end: 450, anchor: FromFork, class: ClassStraddle},
		},
	} {
		profiles["test-"+name] = profile{windows: w, preForkMargin: 390}
		_, err := Resolve("test-"+name, genesis, fork, topo())
		delete(profiles, "test-"+name)
		if err == nil {
			t.Fatalf("%s resolved without error", name)
		}
	}

	profiles["test-straddle"] = profile{
		windows:       []window{{start: -120, end: 60, anchor: FromFork, class: ClassStraddle}},
		preForkMargin: 390,
	}
	t.Cleanup(func() { delete(profiles, "test-straddle") })
	// The anchor must outweigh every island by the proposer-boost margin, or
	// an island could win the heal and never rewind across the fork.
	weak := topo()
	weak.AnchorShare = 0.30 // lights hold 0.233 each: margin 0.067 > 0.05 passes; 0.27 would not
	if _, err := Resolve("test-straddle", genesis, fork, weak); err != nil {
		t.Fatalf("a 30/23 anchor margin was refused: %v", err)
	}
	weak.AnchorShare = 0.27
	if _, err := Resolve("test-straddle", genesis, fork, weak); err == nil {
		t.Fatal("an anchor barely heavier than an island resolved; proposer boost could flip the heal")
	}
	// At or above 2/3 the anchor finalizes its own fork block mid-straddle.
	super := topo()
	super.AnchorShare = 0.70
	if _, err := Resolve("test-straddle", genesis, fork, super); err == nil {
		t.Fatal("a 70% anchor resolved; it would finalize during the split")
	}
	// A single-island straddle is not a straddle: only the victim would revert the fork.
	if err := admitStraddle(Op{Class: ClassStraddle, Victims: []int{2}, Start: at(fork, -120), End: at(fork, 60)}, fork, topo()); err == nil {
		t.Fatal("a one-island straddle passed admission")
	}
}

// A deep island must stall finality without being able to win: its summed
// share stays inside (1/3, 1/2). Two lights at 20% do; one light or three do not.
func TestDeepIslandShare(t *testing.T) {
	if err := admitDeep(Op{Class: ClassDeep, Victims: []int{2, 3}}, topo()); err != nil {
		t.Fatalf("a 40%% pair was refused: %v", err)
	}
	if err := admitDeep(Op{Class: ClassDeep, Victims: []int{2}}, topo()); err == nil {
		t.Fatal("a 20% island passed: the majority would finalize past it")
	}
	if err := admitDeep(Op{Class: ClassDeep, Victims: []int{2, 3, 4}}, topo()); err == nil {
		t.Fatal("a 60% island passed: it could finalize its own branch")
	}
	one := topo()
	one.Lights = []int{2}
	one.Participants = 2
	if _, err := Resolve(preForkFixture(t), time.Unix(1, 0), at(time.Unix(1, 0), 1800), one); err == nil {
		t.Fatal("a deep profile resolved with a single light; a deep island is a pair")
	}
}

// Every shipped profile must keep its pre-fork ops clear of the straddle's
// window: this is the invariant that makes a separate exclusivity check
// unnecessary, so it is asserted directly rather than assumed.
func TestProfilesRespectStraddleWindow(t *testing.T) {
	genesis := time.Unix(4_500_000, 0)
	fork := at(genesis, 1800)
	for _, name := range Names() {
		s, err := Resolve(name, genesis, fork, topo())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		str, ok := s.Straddle()
		if !ok {
			continue
		}
		quietTo := str.Latest().Add(120 * time.Second)
		for _, o := range s.Ops {
			if o.Class == ClassStraddle || !o.Admitted() {
				continue
			}
			if o.End.After(str.Start) && o.Start.Before(quietTo) {
				t.Fatalf("%s: op %s [%s,%s] overlaps the straddle window [%s,%s]",
					name, o.Name, o.Start, o.End, str.Start, quietTo)
			}
		}
	}
}
