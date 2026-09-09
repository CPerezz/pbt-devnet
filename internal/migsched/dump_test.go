package migsched

import (
	"testing"
	"time"
)

// A pre-fork op heals a moment after its scheduled end, so its deadline must
// be the instant the network has to be whole again - not the end itself, or
// no op whose end coincides with a sweep could ever pass.
func TestHealDeadlineIsTheSweepNotTheOpEnd(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	fork := genesis.Add(780 * time.Second)
	s, err := Resolve("composite-smoke", genesis, fork, Topology{
		Anchor: 1, Lights: []int{2, 3, 4}, Participants: 4, AnchorShare: 0.4, SecondsPerSlot: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := s.NewDump(genesis, fork, 2)

	var short, straddle DumpOp
	for _, o := range d.Admitted() {
		if Class(o.Class) == ClassShort {
			short = o
		}
		if Class(o.Class) == ClassStraddle {
			straddle = o
		}
	}
	if short.Name == "" || straddle.Name == "" {
		t.Fatalf("expected a short and a straddle in the dump, got %+v", d.Ops)
	}

	shortDeadline := d.HealDeadline(short)
	if !shortDeadline.After(time.Unix(short.End, 0)) {
		t.Fatalf("the short's deadline %s is not after its end %s: healing on time would be impossible",
			shortDeadline, time.Unix(short.End, 0))
	}
	if shortDeadline.After(fork) {
		t.Fatalf("the short's deadline %s is past the fork %s", shortDeadline, fork)
	}
	// A heal recorded one second after the scheduled end must pass.
	if healedAt := time.Unix(short.End+1, 0); healedAt.After(shortDeadline) {
		t.Fatalf("a heal at %s missed the deadline %s", healedAt, shortDeadline)
	}

	// The straddle heals after the fork by design.
	if sd := d.HealDeadline(straddle); !sd.After(fork) {
		t.Fatalf("the straddle's deadline %s is not after the fork; it heals past it by design", sd)
	}
}
