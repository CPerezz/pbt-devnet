package main

import (
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// The heavy participant takes the deep and straddle windows; every other
// unprotected client rotates through the shorts. Participant 1 is the
// bootnode in every profile the package ships, so it arrives protected.
func TestTopology(t *testing.T) {
	names := []string{"el-1", "el-2", "el-3", "el-4"}
	topo, err := topology(names, []int{1}, 2, 0.40, 6)
	if err != nil {
		t.Fatal(err)
	}
	if topo.Heavy != 2 {
		t.Fatalf("heavy = %d, want 2", topo.Heavy)
	}
	if len(topo.Lights) != 2 || topo.Lights[0] != 3 || topo.Lights[1] != 4 {
		t.Fatalf("lights = %v, want [3 4]: the bootnode and the heavy node must be excluded", topo.Lights)
	}
}

// A heavy node that is also protected is a contradiction: the deep windows
// would have no victim and the profile would quietly run without them.
func TestTopologyRejectsProtectedHeavy(t *testing.T) {
	if _, err := topology([]string{"a", "b"}, []int{2}, 2, 0.4, 6); err == nil {
		t.Fatal("a protected heavy participant was accepted")
	}
	if _, err := topology([]string{"a", "b"}, []int{1}, 5, 0.4, 6); err == nil {
		t.Fatal("a heavy index beyond the configured clients was accepted")
	}
}

// The majority group is everyone outside the victim set: the partition is
// applied as majority-versus-victim, so a missing member would leave that
// node connected to neither side.
func TestOthers(t *testing.T) {
	got := others(4, []int{3})
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("others = %v, want [1 2 4]", got)
	}
}

// The window detail carries the class, which the verifier keys its heal
// deadlines off.
func TestWindowDetailCarriesClass(t *testing.T) {
	topo := migsched.Topology{Heavy: 2, Lights: []int{3}, HeavyShare: 0.4, SecondsPerSlot: 6}
	genesis := time.Unix(1_000_000, 0)
	s, err := migsched.Resolve("composite-smoke", genesis, genesis.Add(780*time.Second), topo)
	if err != nil {
		t.Fatal(err)
	}
	str, ok := s.Straddle()
	if !ok {
		t.Fatal("composite-smoke has no straddle")
	}
	if d := window(str); d == "" || d[:6] != "class=" {
		t.Fatalf("window detail %q does not lead with the class", d)
	}
}
