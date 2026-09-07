package main

import (
	"reflect"
	"testing"
)

// advance feeds n rounds where every client except the frozen ones gains a block.
func advance(t *headTracker, rounds int, clients []string, frozen map[string]bool, start uint64) uint64 {
	h := start
	for i := 0; i < rounds; i++ {
		h++
		sample := map[string]uint64{}
		for _, c := range clients {
			if frozen[c] {
				sample[c] = start
				continue
			}
			sample[c] = h
		}
		t.observe(sample)
	}
	return h
}

func TestFrozenClientIsDetectedAfterWedgeTicks(t *testing.T) {
	clients := []string{"el-1", "el-2", "el-3", "el-4"}
	tr := newHeadTracker()
	// wedgeTicks+1 rounds: the first sample is only a baseline, so it yields wedgeTicks stalls.
	advance(tr, wedgeTicks+1, clients, map[string]bool{"el-3": true}, 100)

	got := tr.frozen()
	if want := []string{"el-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen() = %v, want %v", got, want)
	}
}

func TestFrozenNeedsEnoughRounds(t *testing.T) {
	clients := []string{"el-1", "el-2"}
	tr := newHeadTracker()
	// One round short: the first sample only establishes a baseline, so a client cannot be
	// called wedged this early without making the detector trigger-happy on propagation lag.
	advance(tr, wedgeTicks, clients, map[string]bool{"el-2": true}, 50)
	if got := tr.frozen(); len(got) != 0 {
		t.Fatalf("frozen() = %v, want none before wedgeTicks rounds", got)
	}
}

func TestGloballyStalledChainNamesNobody(t *testing.T) {
	tr := newHeadTracker()
	// Nothing proposes: every head repeats. That is a stalled devnet, not a wedged client,
	// and naming all three here would be a false finding.
	for i := 0; i < wedgeTicks*3; i++ {
		tr.observe(map[string]uint64{"el-1": 7, "el-2": 7, "el-3": 7})
	}
	if got := tr.frozen(); len(got) != 0 {
		t.Fatalf("frozen() = %v, want none for a stalled chain", got)
	}
}

func TestResumingClientClearsItsStall(t *testing.T) {
	clients := []string{"el-1", "el-2"}
	tr := newHeadTracker()
	h := advance(tr, wedgeTicks-1, clients, map[string]bool{"el-2": true}, 10)
	// el-2 catches up before the threshold, so the counter has to reset rather than carry.
	h++
	tr.observe(map[string]uint64{"el-1": h, "el-2": h})
	h = advance(tr, wedgeTicks-1, clients, map[string]bool{"el-2": true}, h)
	if got := tr.frozen(); len(got) != 0 {
		t.Fatalf("frozen() = %v, want none after the client caught up", got)
	}
}

func TestAllButOneFrozenIsReportedInOrder(t *testing.T) {
	clients := []string{"el-1", "el-2", "el-3"}
	tr := newHeadTracker()
	advance(tr, wedgeTicks+1, clients, map[string]bool{"el-3": true, "el-1": true}, 200)
	got := tr.frozen()
	if want := []string{"el-1", "el-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen() = %v, want %v (sorted)", got, want)
	}
}

func TestParticipantIndexTiesTheTwoHalvesOfANode(t *testing.T) {
	for name, want := range map[string]string{
		"el-3-besu-lighthouse": "3",
		"cl-3-lighthouse-besu": "3",
		"el-12-geth-teku":      "12",
		"weird":                "",
	} {
		if got := participantIndex(name); got != want {
			t.Errorf("participantIndex(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAFrozenClientWithNoPeersIsStarvedNotWedged(t *testing.T) {
	// A consensus client with no peers receives no blocks, so its execution client stands
	// still for a reason that is not the client's fault. Reporting that as a finding produced
	// one per scenario on a devnet whose peering had degraded.
	peers := map[string]int{"1": 0, "2": 3, "3": 2, "4": 2}
	wedged, starved := classifyFrozen([]string{"el-1-geth-lighthouse"}, peers)
	if len(wedged) != 0 {
		t.Fatalf("wedged = %v, want none: the client had no peers", wedged)
	}
	if len(starved) != 1 {
		t.Fatalf("starved = %v, want the one client", starved)
	}
}

func TestAFrozenClientWithPeersIsWedged(t *testing.T) {
	peers := map[string]int{"1": 3, "2": 3}
	wedged, starved := classifyFrozen([]string{"el-1-geth-lighthouse"}, peers)
	if len(wedged) != 1 || len(starved) != 0 {
		t.Fatalf("wedged=%v starved=%v, want the client reported as wedged", wedged, starved)
	}
}

func TestAnUnknownPeerCountErrsTowardReporting(t *testing.T) {
	wedged, starved := classifyFrozen([]string{"el-9-geth-lighthouse"}, map[string]int{"1": 3})
	if len(wedged) != 1 || len(starved) != 0 {
		t.Fatalf("wedged=%v starved=%v, want an unknown peer count reported rather than excused",
			wedged, starved)
	}
}

func TestRotationSkipsProtectedNodes(t *testing.T) {
	// Participant 1 is ethereum-package's sole consensus bootnode and has no boot nodes of
	// its own, so stranding it costs the whole devnet its discovery path.
	c := &chaos{els: make([]*el, 5), cfg: config{protected: map[int]bool{1: true}}}

	seen := map[int]int{}
	for i := 0; i < 12; i++ {
		n := c.nextMinority()
		if n == 1 {
			t.Fatalf("rotation returned the protected node on turn %d", i)
		}
		seen[n]++
	}
	if len(seen) != 4 {
		t.Fatalf("rotation covered %d nodes, want all 4 unprotected ones: %v", len(seen), seen)
	}
	for n, count := range seen {
		if count != 3 {
			t.Errorf("node %d chosen %d times, want an even 3 across 12 turns", n, count)
		}
	}
}

func TestEligibleIsEveryNodeWhenNothingIsProtected(t *testing.T) {
	c := &chaos{els: make([]*el, 4), cfg: config{protected: map[int]bool{}}}
	if got := c.eligible(); len(got) != 4 {
		t.Fatalf("eligible() = %v, want all four nodes", got)
	}
}
