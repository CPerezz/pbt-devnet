package migsched

import (
	"testing"
	"time"
)

// A four-node topology: participant 1 is the bootnode (never disruptable,
// so absent here), 2 holds 40% of the validators, 3 and 4 the rest.
func topo() Topology {
	return Topology{Heavy: 2, Lights: []int{3, 4}, HeavyShare: 0.40, SecondsPerSlot: 6}
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
// refusal and sweep placement: three deeps on the heavy victim, then two
// shorts rotating over the lights. Injected because no shipped profile is
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

// The pre-fork shape: deeps pin the heavy victim, shorts rotate over the
// lights, everything is admitted with room to spare.
func TestResolvePreFork(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	fork := at(genesis, 1800)
	s, err := Resolve(preForkFixture(t), genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start, end int
		victim     int
		class      Class
	}{
		{240, 430, 2, ClassDeep},
		{540, 730, 2, ClassDeep},
		{840, 1030, 2, ClassDeep},
		{1090, 1240, 3, ClassShort},
		{1300, 1450, 4, ClassShort},
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
		if o.Class != w.class || len(o.Victims) != 1 || o.Victims[0] != w.victim {
			t.Fatalf("op %d = %s on %v, want %s on %d", i, o.Class, o.Victims, w.class, w.victim)
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
// the last pre-fork end, the pre-fork deadline, the straddle's heal, its
// repeat a minute later, and the window op's repeat.
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
	if !str.Start.Equal(at(fork, -90)) || !str.End.Equal(at(fork, 90)) {
		t.Fatalf("straddle = [%s,%s], want fork-90..fork+90", str.Start, str.End)
	}
	if str.Victims[0] != 2 {
		t.Fatalf("straddle victim = %v, want the heavy participant", str.Victims)
	}
	// The window op lives inside the open migration window, on a light
	// victim that is NOT the straddle's: the mid-migration disruption and
	// the boundary rewind must land on different nodes to be separable.
	var win *Op
	for i := range s.Ops {
		if s.Ops[i].Class == ClassWindow {
			win = &s.Ops[i]
		}
	}
	if win == nil {
		t.Fatal("composite resolved without a window op")
	}
	if !win.Start.Equal(at(fork, 240)) || !win.End.Equal(at(fork, 390)) {
		t.Fatalf("window op = [%s,%s], want fork+240..fork+390", win.Start, win.End)
	}
	if len(win.Victims) != 1 || win.Victims[0] == str.Victims[0] {
		t.Fatalf("window victims = %v, want one light distinct from the straddle victim %v", win.Victims, str.Victims)
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
		at(genesis, 940), at(genesis, 1410), at(fork, 90), at(fork, 150), at(fork, 450),
	})
	if !s.Quiet.Equal(at(fork, 510)) {
		t.Fatalf("quiet = %s, want fork+510 (window op end + quiet tail)", s.Quiet)
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
	for _, probe := range []int{-89, -30, 0, 30, 89, 90, 150, 209, 250, 380, 500} {
		if !s.Covers(at(fork, probe)) {
			t.Fatalf("fork%+ds reads as illegal divergence; a watchdog would cut the op short", probe)
		}
	}
	// Past the window op's heal allowance it must stop being legal, or a
	// genuinely wedged node would never be noticed. The straddle's own
	// tail ends at fork+210; fork+230 sits in the gap before the window
	// op opens at fork+240.
	for _, probe := range []int{230, 540} {
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
		"too-little-before": {{start: -30, end: 90, anchor: FromFork, class: ClassStraddle}},
		"too-little-after":  {{start: -90, end: 30, anchor: FromFork, class: ClassStraddle}},
		"too-long-after":    {{start: -90, end: 200, anchor: FromFork, class: ClassStraddle}},
		"two-straddles": {
			{start: -90, end: 90, anchor: FromFork, class: ClassStraddle},
			{start: 300, end: 480, anchor: FromFork, class: ClassStraddle},
		},
	} {
		profiles["test-"+name] = profile{windows: w, preForkMargin: 390}
		_, err := Resolve("test-"+name, genesis, fork, topo())
		delete(profiles, "test-"+name)
		if err == nil {
			t.Fatalf("%s resolved without error", name)
		}
	}

	// A share so heavy the expected dropped branch exceeds the bound.
	heavy := topo()
	heavy.HeavyShare = 0.95
	profiles["test-straddle"] = profile{
		windows:       []window{{start: -90, end: 90, anchor: FromFork, class: ClassStraddle}},
		preForkMargin: 390,
	}
	t.Cleanup(func() { delete(profiles, "test-straddle") })
	if _, err := Resolve("test-straddle", genesis, fork, heavy); err == nil {
		t.Fatal("a 95% victim share resolved; the heal rewind would be unbounded")
	}
	// The victim rule is defensive - Resolve pins the heavy node itself -
	// so exercise it directly.
	if err := admitStraddle(Op{Class: ClassStraddle, Victims: []int{3}, Start: at(fork, -90), End: at(fork, 90)}, fork, topo()); err == nil {
		t.Fatal("a straddle on a light victim passed admission")
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
		quietTo := str.End.Add(120 * time.Second)
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

func TestTopologyGuards(t *testing.T) {
	genesis := time.Unix(5_000_000, 0)
	fork := at(genesis, 1800)
	for name, mut := range map[string]func(*Topology){
		"no heavy":     func(tp *Topology) { tp.Heavy = 0 },
		"no lights":    func(tp *Topology) { tp.Lights = nil },
		"no slot time": func(tp *Topology) { tp.SecondsPerSlot = 0 },
	} {
		tp := topo()
		mut(&tp)
		if _, err := Resolve("composite", genesis, fork, tp); err == nil {
			t.Fatalf("composite resolved with %s", name)
		}
	}
}

// composite-smoke shows all three phases in one short lap: a light short
// before the fork, then the straddle.
func TestResolveCompositeSmoke(t *testing.T) {
	genesis := time.Unix(5_500_000, 0)
	fork := at(genesis, 780)
	s, err := Resolve("composite-smoke", genesis, fork, topo())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(s.Ops))
	}
	first := s.Ops[0]
	if first.Class != ClassShort || first.Victims[0] != 3 {
		t.Fatalf("first op = %s on %v, want a short on a light node", first.Class, first.Victims)
	}
	if !first.Admitted() {
		t.Fatalf("the pre-fork short was refused: %s", first.Refused)
	}
	if deadline := at(fork, -390); first.End.After(deadline) {
		t.Fatalf("short ends %s, past the %s deadline", first.End, deadline)
	}
	if _, ok := s.Straddle(); !ok {
		t.Fatal("composite-smoke resolved without a straddle")
	}
}

func TestUnknownProfile(t *testing.T) {
	if _, err := Resolve("nope", time.Unix(1, 0), time.Unix(2, 0), topo()); err == nil {
		t.Fatal("unknown profile resolved")
	}
}
