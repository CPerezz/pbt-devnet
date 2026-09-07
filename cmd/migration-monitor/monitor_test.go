package main

import (
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// A split that heals inside the grace must stay silent: every partition the
// schedules hold is shorter than this, and reporting one as a fault would
// fail every chaos-bearing run.
func TestSplitWatchToleratesPartitions(t *testing.T) {
	w := &splitWatch{}
	start := time.Unix(1_000_000, 0)
	for _, offset := range []time.Duration{0, time.Minute, 3 * time.Minute, splitGrace} {
		if e := w.observe(start.Add(offset), true, 100, "a vs b"); e != nil {
			t.Fatalf("split at +%s fired inside the grace: %+v", offset, e)
		}
	}
	if e := w.observe(start.Add(splitGrace+time.Minute), false, 100, ""); e != nil {
		t.Fatalf("a healed split fired: %+v", e)
	}
	// After healing, the timer restarts rather than carrying the old age.
	if e := w.observe(start.Add(splitGrace+2*time.Minute), true, 100, "a vs b"); e != nil {
		t.Fatalf("a fresh split fired immediately after an earlier one healed: %+v", e)
	}
}

// A split that outlives every window is a node that cannot rewind, which is
// the failure a reorg spanning the activation risks.
func TestSplitWatchFiresOnceWhenStuck(t *testing.T) {
	w := &splitWatch{}
	start := time.Unix(2_000_000, 0)
	w.observe(start, true, 42, "a=0x1 vs b=0x2")
	e := w.observe(start.Add(splitGrace+time.Second), true, 42, "a=0x1 vs b=0x2")
	if e == nil || e.Finding != migmon.FindingNoConvergence || e.Kind != migmon.EvCritical {
		t.Fatalf("want a no-convergence critical, got %+v", e)
	}
	if e.Number != 42 {
		t.Fatalf("finding lost the height: %+v", e)
	}
	if again := w.observe(start.Add(splitGrace+time.Minute), true, 42, "a=0x1 vs b=0x2"); again != nil {
		t.Fatalf("the finding repeated: %+v", again)
	}
}
