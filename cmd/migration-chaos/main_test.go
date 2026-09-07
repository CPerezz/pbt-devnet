package main

import (
	"testing"
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
