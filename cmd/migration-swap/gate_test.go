package main

import (
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// --- tooLate: deadline/skip arithmetic -------------------------------------------------

func TestTooLate(t *testing.T) {
	cases := []struct {
		name                        string
		now, fork, expected, margin int64
		want                        bool
	}{
		{"plenty of room", 1000, 2000, 240, 300, false},
		{"exactly on the deadline still fits", 1000, 1000 + 240 + 300, 240, 300, false},
		{"one second past the deadline", 1000, 1000 + 240 + 300 - 1, 240, 300, true},
		{"already past the fork", 2500, 2000, 240, 300, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tooLate(c.now, c.fork, c.expected, c.margin); got != c.want {
				t.Fatalf("tooLate(%d,%d,%d,%d) = %v, want %v", c.now, c.fork, c.expected, c.margin, got, c.want)
			}
		})
	}
}

// --- chaosFree: Dump gap lookup ---------------------------------------------------------

func TestChaosFree(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	dump := migsched.Dump{
		Genesis: base.Unix(), Fork: base.Add(1000 * time.Second).Unix(), SlotSeconds: 6,
		Ops: []migsched.DumpOp{
			{Name: "deep-1", Class: "deep", Start: base.Add(100 * time.Second).Unix(), End: base.Add(300 * time.Second).Unix(), Victims: []int{2, 3}},
		},
	}
	cases := []struct {
		name    string
		now     time.Time
		horizon time.Duration
		want    int64
	}{
		{"well inside the gap before the op", base, gapHorizon, 100},
		{"zero: now sits inside the busy window", base.Add(150 * time.Second), gapHorizon, 0},
		{"past the op, gap capped by the horizon", base.Add(300 * time.Second), 700 * time.Second, 700},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chaosFree(dump, c.now, c.horizon); got != c.want {
				t.Fatalf("chaosFree(%s, %s) = %d, want %d", c.now, c.horizon, got, c.want)
			}
		})
	}
}

// --- exportBlocker: every failing export condition blocks, all hold -> go --------------

func TestExportBlocker(t *testing.T) {
	base := exportState{Now: 200, NotBefore: 100, Partitions: 0, Gap: 200, Down: nil}
	cases := []struct {
		name   string
		mutate func(s exportState) exportState
		want   string // substring, "" means must be empty (go)
	}{
		{"all hold", func(s exportState) exportState { return s }, ""},
		{"too early", func(s exportState) exportState { s.Now = 50; return s }, "not before"},
		{"disruptoor unreachable", func(s exportState) exportState { s.Partitions = -1; return s }, "unknown"},
		{"partition applied", func(s exportState) exportState { s.Partitions = 1; return s }, "partitions applied"},
		{"gap too short", func(s exportState) exportState { s.Gap = 10; return s }, "need 180"},
		{"el not answering", func(s exportState) exportState { s.Down = []string{"el-4-besu-lighthouse"}; return s }, "not answering"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := exportBlocker(c.mutate(base))
			if c.want == "" && got != "" {
				t.Fatalf("exportBlocker() = %q, want go (empty)", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("exportBlocker() = %q, want it to mention %q", got, c.want)
			}
		})
	}
}

// --- decideSwap: the swap gate predicate -------------------------------------------------

func baseSwapState() swapState {
	return swapState{
		Now: 1000, Fork: 10000, Expected: 240, Margin: 300,
		Anchor: 500, AnchorHash: "0xabc",
		Finalized:    &migmon.Header{Number: 600, Time: 940},
		Canonical:    "0xabc",
		EpochSeconds: 192,
		Down:         nil, Partitions: 0, Gap: 1000,
		PrevBehind: false, PrevFailed: false,
	}
}

func TestDecideSwapEveryHoldingConditionProceeds(t *testing.T) {
	v, reason := decideSwap(baseSwapState())
	if v != proceed {
		t.Fatalf("decideSwap() = %v (%q), want proceed", v, reason)
	}
}

func TestDecideSwapEachFailingConditionBlocks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s swapState) swapState
		want   verdict
	}{
		{"too late", func(s swapState) swapState { s.Now = s.Fork - s.Expected - s.Margin + 1; return s }, skip},
		{"anchor not finalized yet", func(s swapState) swapState { s.Finalized.Number = 100; return s }, wait},
		{"finality unknown", func(s swapState) swapState { s.Finalized = nil; return s }, wait},
		{"anchor hash unknown on node 1", func(s swapState) swapState { s.Canonical = ""; return s }, wait},
		{"finality stale", func(s swapState) swapState { s.Now = int64(s.Finalized.Time) + 4*s.EpochSeconds + 1; return s }, wait},
		{"other validator down", func(s swapState) swapState { s.Down = []string{"el-4-besu-lighthouse"}; return s }, wait},
		{"disruptoor unreachable", func(s swapState) swapState { s.Partitions = -1; return s }, wait},
		{"partition applied", func(s swapState) swapState { s.Partitions = 1; return s }, wait},
		{"gap too short", func(s swapState) swapState { s.Gap = 10; return s }, wait},
		{"previous consumer not caught up", func(s swapState) swapState { s.PrevBehind = true; return s }, wait},
		{"reorged anchor", func(s swapState) swapState { s.Canonical = "0xdead"; return s }, reorged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, reason := decideSwap(c.mutate(baseSwapState()))
			if v != c.want {
				t.Fatalf("decideSwap() = %v (%q), want %v", v, reason, c.want)
			}
			if reason == "" {
				t.Fatalf("decideSwap() gave no reason for a blocking verdict")
			}
		})
	}
}

// A previous swap's failure only forces a skip once finality has actually gone stale;
// while finality is still fresh the gate keeps waiting on the predecessor instead.
func TestDecideSwapPrevFailedOnlySkipsOnceFinalityIsStale(t *testing.T) {
	s := baseSwapState()
	s.PrevFailed = true
	if v, _ := decideSwap(s); v != proceed {
		t.Fatalf("decideSwap() with fresh finality = %v, want proceed (prevFailed alone does not block)", v)
	}
	s.Now = int64(s.Finalized.Time) + 4*s.EpochSeconds + 1
	if v, _ := decideSwap(s); v != skip {
		t.Fatalf("decideSwap() with stale finality after a prior failure = %v, want skip", v)
	}
}

// tooLate is checked before everything else: a reorg discovered one second before the
// deadline still skips as "too late", not "reorged" - the producer is still marked failed
// by the caller regardless, but the swap's own reason should be the one that actually fired.
func TestDecideSwapTooLateBeatsEveryOtherCheck(t *testing.T) {
	s := baseSwapState()
	s.Canonical = "0xdead" // would otherwise be reorged
	s.Now = s.Fork - s.Expected - s.Margin + 1
	if v, _ := decideSwap(s); v != skip {
		t.Fatalf("decideSwap() = %v, want skip (too late wins over reorged)", v)
	}
}

// --- parseMeta: erigon pbt-snapshot.meta.json -------------------------------------------

func TestParseMeta(t *testing.T) {
	cases := []struct {
		raw string
		ok  bool
	}{
		{`{"block":812,"blockHash":"0xaaa","stateRoot":"0xbbb","pbtRoot":"0xccc"}`, true},
		{`{"blockHash":"0xaaa","pbtRoot":"0xccc"}`, false}, // no block
		{`{"block":812,"pbtRoot":"0xccc"}`, false},         // no blockHash
		{`{"block":812,"blockHash":"0xaaa"}`, false},       // no pbtRoot
		{`not json`, false},
	}
	for _, c := range cases {
		m, err := parseMeta([]byte(c.raw))
		if (err == nil) != c.ok {
			t.Fatalf("parseMeta(%s) err = %v, want ok=%v", c.raw, err, c.ok)
		}
		if c.ok && m.Block != 812 {
			t.Fatalf("parseMeta(%s).Block = %d, want 812", c.raw, m.Block)
		}
	}
}

func TestNewEvidenceRejectsUnknownProducerKind(t *testing.T) {
	cfg := migmon.Offline{Enabled: true,
		Producers: []migmon.OfflineProducer{{Node: 6, Kind: "something-else"}},
		Consumers: []migmon.OfflineConsumer{{Node: 5}},
	}
	if _, err := newEvidence(cfg); err == nil {
		t.Fatal("newEvidence() with an unknown producer kind succeeded, want an error")
	}
}

func TestNewEvidenceRejectsOrphanConsumer(t *testing.T) {
	cfg := migmon.Offline{Enabled: true,
		Producers: []migmon.OfflineProducer{{Node: 6, Kind: "geth-convert", Consumers: []int{5}}},
		Consumers: []migmon.OfflineConsumer{{Node: 3}}, // node 3's producer (2) is not in Producers
	}
	if _, err := newEvidence(cfg); err == nil {
		t.Fatal("newEvidence() with a consumer that has no producer succeeded, want an error")
	}
}
