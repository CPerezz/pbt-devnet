package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

func rpcKey(url, method string, params ...any) string {
	return url + "|" + method + "|" + fmt.Sprintf("%v", params)
}

// fakeFetcher answers canned responses keyed by rpcKey, and fails the test
// on an unexpected call — RPC-dependent checks are factored through the
// `fetcher` type precisely so tests never touch a network.
func fakeFetcher(t *testing.T, responses map[string]json.RawMessage) fetcher {
	t.Helper()
	return func(ctx context.Context, url, method string, out any, params ...any) error {
		raw, ok := responses[rpcKey(url, method, params...)]
		if !ok {
			t.Fatalf("fake fetcher: no response stubbed for %s", rpcKey(url, method, params...))
		}
		return json.Unmarshal(raw, out)
	}
}

func ev(kind, node string, tm int64) migmon.Event {
	return migmon.Event{Kind: kind, Node: node, Time: time.Unix(tm, 0).UTC()}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- schedule-driven fixtures shared by C3/C9/C10 -----------------------

const testFork = int64(100000)

// straddleTestOps is a schedule with three pre-fork isolations (two deep,
// one short) and a straddle spanning testFork, one victim each -
// participant indices 1-4 so chaos events can name them "node-<i>" and
// attributeWindows can resolve them back to these ops.
func straddleTestOps() []migsched.DumpOp {
	return []migsched.DumpOp{
		{Name: "pre1", Class: "deep", Start: 1000, End: 1100, Victims: []int{1}},
		{Name: "pre2", Class: "short", Start: 2000, End: 2100, Victims: []int{2}},
		{Name: "pre3", Class: "deep", Start: 3000, End: 3100, Victims: []int{3}},
		{Name: "straddle1", Class: "straddle", Start: testFork - 50, End: testFork + 50, Victims: []int{4}},
	}
}

func scheduleEvent(t *testing.T, d migsched.Dump) migmon.Event {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return migmon.Event{Kind: migmon.EvSchedule, Time: time.Unix(d.Genesis, 0).UTC(), Raw: raw}
}

func reorgEv(node string, tm int64, depth int) migmon.Event {
	e := ev(migmon.EvReorg, node, tm)
	e.Detail = fmt.Sprintf("0xold -> 0xnew (depth %d)", depth)
	return e
}

// baselineChaosAndMonitor isolates and heals all four straddleTestOps
// victims on time, each corroborated by a reorg - the fixture every
// C3/C9 variant below tweaks from. Indices: 0/1 = pre1 isolate/heal,
// 2/3 = pre2, 4/5 = pre3, 6/7 = straddle1; monitor 0-3 are their reorgs.
func baselineChaosAndMonitor() (chaos, monitor []migmon.Event) {
	chaos = []migmon.Event{
		ev(migmon.EvIsolate, "node-1", 1000), ev(migmon.EvHeal, "node-1", 1050),
		ev(migmon.EvIsolate, "node-2", 2000), ev(migmon.EvHeal, "node-2", 2050),
		ev(migmon.EvIsolate, "node-3", 3000), ev(migmon.EvHeal, "node-3", 3050),
		ev(migmon.EvIsolate, "node-4", testFork-50), ev(migmon.EvHeal, "node-4", testFork-10),
	}
	monitor = []migmon.Event{
		reorgEv("node-1", 1060, 5),
		reorgEv("node-2", 2060, 6),
		reorgEv("node-3", 3060, 7),
		reorgEv("node-4", testFork-5, 8),
	}
	return chaos, monitor
}

func withSchedule(t *testing.T, dump migsched.Dump, chaos []migmon.Event) []migmon.Event {
	t.Helper()
	return append([]migmon.Event{scheduleEvent(t, dump)}, chaos...)
}

// --- C3: schedule-derived expectations and per-class deadlines ----------

func TestCheckC3ScheduleDrivenDeadlines(t *testing.T) {
	// Quiet is what tells a post-migration isolation from a stray one, and
	// the driver always publishes it.
	dump := migsched.Dump{Profile: "straddle-fixture", Fork: testFork, Heavy: 4, Ops: straddleTestOps(), Quiet: testFork + 210}

	t.Run("baseline: all four heal on time, fully corroborated, passes", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	// This is the case the old global T-300 rule failed by construction:
	// a straddle heals after the fork itself by design.
	t.Run("straddle heals after the fork but inside its own deadline: still passes", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		chaos[7].Time = time.Unix(testFork+60, 0).UTC() // straddle heal, well after the fork
		monitor[3] = reorgEv("node-4", testFork+65, 8)
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (straddle's deadline is End+120s, not the fork), got %s: %s", r, evidence)
		}
	})

	t.Run("pre-fork op heals after its own deadline (the fork itself): fails", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		chaos[1].Time = time.Unix(testFork+10, 0).UTC() // pre1 heal, past the fork
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (pre-fork op healed past its deadline), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "deadline") {
			t.Fatalf("evidence %q missing deadline reason", evidence)
		}
	})

	t.Run("healed but unmatched op is inconclusive, not fail (stochastic miss)", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		monitor = append(monitor[:2], monitor[3]) // drop pre3's reorg only: 3/4 corroborated
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive (4 healed, 3 corroborated), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "pre3") {
			t.Fatalf("evidence %q should name the unmatched op", evidence)
		}
	})

	t.Run("isolation not attributable to any admitted op fails", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		// Inside the schedule's active period, so it is a partition
		// nobody scheduled - not the gate's post-migration window.
		chaos = append(chaos, ev(migmon.EvIsolate, "node-9", testFork-1000), ev(migmon.EvHeal, "node-9", testFork-950))
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (unscheduled isolation), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "not in the schedule") {
			t.Fatalf("evidence %q missing unscheduled reason", evidence)
		}
	})

	t.Run("post-migration isolation after the quiet instant is noted, not failed", func(t *testing.T) {
		// The gate applies its own window once the schedule is finished;
		// it is deliberately absent from the pre-fork schedule, and the
		// boundary and orphan checks judge its convergence.
		chaos, monitor := baselineChaosAndMonitor()
		chaos = append(chaos,
			ev(migmon.EvIsolate, "node-3", testFork+400),
			ev(migmon.EvHeal, "node-3", testFork+560))
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "post-migration") {
			t.Fatalf("evidence %q does not record the post-migration isolation", evidence)
		}
	})

	t.Run("unhealed admitted op fails", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		chaos = chaos[:len(chaos)-1] // drop node-4's heal
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (never healed), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "never healed") {
			t.Fatalf("evidence %q missing never-healed reason", evidence)
		}
	})

	t.Run("all admitted ops healed but fewer than a full run schedules is inconclusive", func(t *testing.T) {
		// A profile that only schedules three partitions cannot produce
		// four; that makes the evidence thin, not the run broken.
		three := migsched.Dump{Profile: "three", Fork: testFork, Heavy: 4, Quiet: testFork + 210,
			Ops: straddleTestOps()[:3]}
		chaos, monitor := baselineChaosAndMonitor()
		chaos = chaos[:6] // only the three ops this schedule admits
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, three, chaos), monitor: monitor}
		r, evidence := v.checkC3(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive (thin coverage), got %s: %s", r, evidence)
		}
	})

	t.Run("missing schedule record fails: the run's own schedule is mandatory input", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		v := &verifier{T: uint64(testFork), chaos: chaos, monitor: monitor} // no schedule event
		r, evidence := v.checkC3(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (no schedule record), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "schedule") {
			t.Fatalf("evidence %q missing schedule reason", evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass even with no schedule", func(t *testing.T) {
		v := &verifier{T: uint64(testFork), skipChaos: true}
		r, evidence := v.checkC3(context.Background())
		if r != verdictPass || !strings.Contains(evidence, "skipped") {
			t.Fatalf("want skipped pass, got %s evidence=%q", r, evidence)
		}
	})
}

// --- C9: deep reorg, split out of C3's pass/fail -------------------------

func TestCheckC9DeepReorg(t *testing.T) {
	dump := migsched.Dump{Profile: "p", Fork: testFork, Ops: straddleTestOps()}

	t.Run("no isolation reaches depth 10: inconclusive, not fail", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor() // max depth 8
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC9(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("one isolation reaches depth >= 10: passes", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		monitor[2] = reorgEv("node-3", 3060, 12)
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkC9(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "pre3") {
			t.Fatalf("evidence %q should name the deep op", evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass", func(t *testing.T) {
		v := &verifier{skipChaos: true}
		r, _ := v.checkC9(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s", r)
		}
	})
}

// --- C10: the straddle actually happened ---------------------------------

var (
	testOldHash = "0x" + strings.Repeat("a", 64)
	testNewHash = "0x" + strings.Repeat("b", 64)
	zeroRoot    = "0x" + strings.Repeat("0", 64)
)

func bstarReorgedEv(node string, height uint64, oldHash, newHash string, tm int64) migmon.Event {
	e := ev(migmon.EvBStarReorged, node, tm)
	e.Number = height
	e.Hash = newHash
	e.Detail = fmt.Sprintf("fork block %d %s was orphaned; height %d now holds %s", height, oldHash, height, newHash)
	return e
}

func TestCheckC10Straddle(t *testing.T) {
	straddleOp := migsched.DumpOp{Name: "straddle1", Class: "straddle", Start: testFork - 50, End: testFork + 50, Victims: []int{4}}
	nonStraddleOnly := migsched.Dump{Profile: "p", Fork: testFork, Ops: straddleTestOps()[:1]}
	withStraddle := migsched.Dump{Profile: "p", Fork: testFork, Ops: []migsched.DumpOp{straddleOp}}

	t.Run("no straddle admitted: inconclusive", func(t *testing.T) {
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, nonStraddleOnly)}}
		r, evidence := v.checkC10(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted but victim never orphaned: inconclusive", func(t *testing.T) {
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}}
		r, evidence := v.checkC10(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted, victim orphaned, dropped branch >= 6: passes", func(t *testing.T) {
		monitor := []migmon.Event{
			bstarReorgedEv("node-4", 500, testOldHash, testNewHash, testFork+10),
			reorgEv("node-4", testFork+15, 8),
		}
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}, monitor: monitor}
		r, evidence := v.checkC10(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted, victim orphaned, dropped branch < 6: fails", func(t *testing.T) {
		monitor := []migmon.Event{
			bstarReorgedEv("node-4", 500, testOldHash, testNewHash, testFork+10),
			reorgEv("node-4", testFork+15, 3),
		}
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}, monitor: monitor}
		r, evidence := v.checkC10(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass", func(t *testing.T) {
		v := &verifier{skipChaos: true}
		r, _ := v.checkC10(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s", r)
		}
	})
}

// --- C11: boundary convergence -------------------------------------------

func TestCheckC11BoundaryConvergence(t *testing.T) {
	hashA := "0x" + strings.Repeat("a", 64)
	hashB := "0x" + strings.Repeat("b", 64)
	hashC := "0x" + strings.Repeat("c", 64)
	els := []el{{name: "el1", url: "http://el1"}, {name: "el2", url: "http://el2"}}

	finalRecords := []migmon.Event{
		{Kind: migmon.EvBStarFinal, Node: "el1", Number: 500, Hash: hashA, Time: time.Unix(1000, 0)},
		{Kind: migmon.EvBStarFinal, Node: "el2", Number: 500, Hash: hashA, Time: time.Unix(1000, 0)},
	}
	provisionalDiversity := []migmon.Event{
		{Kind: migmon.EvBStar, Node: "el1", Hash: hashB, Time: time.Unix(500, 0)},
		{Kind: migmon.EvBStar, Node: "el2", Hash: hashC, Time: time.Unix(500, 0)},
	}
	agreeingFetch := fakeFetcher(t, map[string]json.RawMessage{
		rpcKey("http://el1", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashA, zeroRoot),
		rpcKey("http://el2", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashA, zeroRoot),
	})

	t.Run("agreement, no F4, >=2 provisional hashes: passes", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		v := &verifier{els: els, monitor: monitor, fetch: agreeingFetch}
		r, evidence := v.checkC11(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("node disagrees with the final fork block: fails", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		fetch := fakeFetcher(t, map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashA, zeroRoot),
			rpcKey("http://el2", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashB, zeroRoot),
		})
		v := &verifier{els: els, monitor: monitor, fetch: fetch}
		r, evidence := v.checkC11(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})

	t.Run("unwaived F4 critical: fails", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		crit := ev(migmon.EvCritical, "el1", 99999)
		crit.Finding = migmon.FindingNoConvergence
		monitor = append(monitor, crit)
		v := &verifier{els: els, monitor: monitor, fetch: agreeingFetch}
		r, evidence := v.checkC11(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "F4") {
			t.Fatalf("evidence %q missing F4 reason", evidence)
		}
	})

	t.Run("F4 waived inside a chaos window does not fail on that alone", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		crit := ev(migmon.EvCritical, "el1", 1500)
		crit.Finding = migmon.FindingNoConvergence
		monitor = append(monitor, crit)
		chaos := []migmon.Event{ev(migmon.EvIsolate, "el1", 1400), ev(migmon.EvHeal, "el1", 1600)}
		v := &verifier{els: els, monitor: monitor, chaos: chaos, fetch: agreeingFetch}
		r, evidence := v.checkC11(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (F4 waived by chaos window), got %s: %s", r, evidence)
		}
	})

	t.Run("fewer than 2 distinct provisional hashes: inconclusive", func(t *testing.T) {
		v := &verifier{els: els, monitor: finalRecords, fetch: agreeingFetch}
		r, evidence := v.checkC11(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("no bstar record observed at all: fails", func(t *testing.T) {
		v := &verifier{els: els}
		r, evidence := v.checkC11(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})
}

// --- C12: the orphaned branch is actually gone ---------------------------

func TestCheckC12OrphanedBranchGone(t *testing.T) {
	els := []el{{name: "el1", url: "http://el1"}, {name: "el2", url: "http://el2"}}
	orphanHash := testOldHash
	monitor := []migmon.Event{bstarReorgedEv("node-4", 500, orphanHash, testNewHash, testFork+10)}

	t.Run("no orphaned block identified: inconclusive", func(t *testing.T) {
		v := &verifier{els: els}
		r, evidence := v.checkC12(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("every node reports the orphan gone (null): passes", func(t *testing.T) {
		fetch := fakeFetcher(t, map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByHash", orphanHash, false): json.RawMessage("null"),
			rpcKey("http://el2", "eth_getBlockByHash", orphanHash, false): json.RawMessage("null"),
		})
		v := &verifier{els: els, monitor: monitor, fetch: fetch}
		r, evidence := v.checkC12(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("orphan still resolvable but non-canonical: passes", func(t *testing.T) {
		fetch := fakeFetcher(t, map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByHash", orphanHash, false): blockJSON(orphanHash, zeroRoot),
			rpcKey("http://el1", "eth_getBlockByNumber", "0x0", false):    blockJSON(testNewHash, zeroRoot), // canonical differs
			rpcKey("http://el2", "eth_getBlockByHash", orphanHash, false): json.RawMessage("null"),
		})
		v := &verifier{els: els, monitor: monitor, fetch: fetch}
		r, evidence := v.checkC12(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (dangling non-canonical block is fine), got %s: %s", r, evidence)
		}
	})

	t.Run("orphan still served as canonical: fails", func(t *testing.T) {
		fetch := fakeFetcher(t, map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByHash", orphanHash, false): blockJSON(orphanHash, zeroRoot),
			rpcKey("http://el1", "eth_getBlockByNumber", "0x0", false):    blockJSON(orphanHash, zeroRoot), // canonical == orphan
			rpcKey("http://el2", "eth_getBlockByHash", orphanHash, false): json.RawMessage("null"),
		})
		v := &verifier{els: els, monitor: monitor, fetch: fetch}
		r, evidence := v.checkC12(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "canonical") {
			t.Fatalf("evidence %q missing canonical reason", evidence)
		}
	})
}

// --- per-client reorg log registry ---------------------------------------

func TestClientOf(t *testing.T) {
	cases := map[string]string{
		"el-5-besu-lighthouse":     "besu",
		"el-2-geth-lighthouse":     "geth",
		"node-2":                   "",
		"el-2-geth-lighthouse.log": "geth",
	}
	for in, want := range cases {
		if got := clientOf(in); got != want {
			t.Errorf("clientOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogReorgMatchesExtractsDropAndAncestor(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "el-7-geth-lighthouse.log")
	writeFile(t, logPath, "INFO Chain reorg detected number=39 hash=6e845d..6e71d0 drop=11 dropfrom=3dc2e1..8b760b add=12\n")
	matches := logReorgMatches(logPath, clientLogPatterns["geth"])
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(matches))
	}
	// number= is go-ethereum's common-ancestor height (core/blockchain.go
	// walks both chains back to their shared parent and logs it as
	// commonBlock), used directly with no arithmetic.
	if matches[0].drop != 11 || matches[0].ancestor != 39 {
		t.Fatalf("want drop=11 ancestor=39, got %+v", matches[0])
	}
}

func TestMatchReorgUnknownClientDegrades(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "el-5-besu-lighthouse.log"), "some unrelated line\n")
	w := chaosWindow{node: "node-5", from: time.Unix(1000, 0), to: time.Unix(1100, 0), healed: true}
	v := &verifier{logsDir: dir}
	got := v.matchReorg(w)
	if got.matched {
		t.Fatalf("want unmatched (no monitor event, unregistered client), got %+v", got)
	}
	if !got.degraded {
		t.Fatalf("want degraded=true for a client with no registered log pattern")
	}
	if !strings.Contains(got.String(), "monitor-events-only") {
		t.Fatalf("evidence %q must say it degraded to monitor-events-only", got.String())
	}
}

func TestMatchReorgRegisteredClientNoMatchIsNotDegraded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "el-6-geth-lighthouse.log"), "no reorg here\n")
	w := chaosWindow{node: "node-6", from: time.Unix(1000, 0), to: time.Unix(1100, 0), healed: true}
	v := &verifier{logsDir: dir}
	got := v.matchReorg(w)
	if got.matched {
		t.Fatalf("want unmatched, got %+v", got)
	}
	if got.degraded {
		t.Fatalf("want NOT degraded: geth has a registered pattern, it just didn't match this log")
	}
}

// --- Run(): exit-code and inconclusive-trailer semantics ------------------

func TestSummarizeVerdicts(t *testing.T) {
	results := []checkResult{
		{id: "C1", verdict: verdictPass},
		{id: "C2", verdict: verdictFail},
		{id: "C3", verdict: verdictInconclusive},
		{id: "C4", verdict: verdictInconclusive},
	}
	failed, inconclusiveIDs := summarizeVerdicts(results)
	if failed != 1 {
		t.Fatalf("want 1 failed, got %d", failed)
	}
	if strings.Join(inconclusiveIDs, ",") != "C3,C4" {
		t.Fatalf("want inconclusive [C3 C4], got %v", inconclusiveIDs)
	}
}

// --- --summary artifact rendering -----------------------------------------

func TestRenderSummary(t *testing.T) {
	dump := migsched.Dump{Profile: "straddle-fixture", Fork: testFork, Heavy: 4, Ops: straddleTestOps()}
	chaos, monitor := baselineChaosAndMonitor()
	monitor = append(monitor,
		ev(migmon.EvProgress, "node-1", testFork+250),
		migmon.Event{Kind: migmon.EvBStar, Node: "node-1", Number: 500, Hash: "0x" + strings.Repeat("a", 64), Time: time.Unix(testFork, 0)},
		migmon.Event{Kind: migmon.EvBStarFinal, Node: "node-1", Number: 500, Hash: "0x" + strings.Repeat("a", 64), Time: time.Unix(testFork+200, 0)},
	)

	v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
	results := []checkResult{
		{id: "C1", verdict: verdictPass, evidence: "b*=500"},
		{id: "C3", verdict: verdictInconclusive, evidence: "3 corroborated"},
		{id: "C10", verdict: verdictFail, evidence: "dropped branch too shallow"},
	}
	out := v.renderSummary(results)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 60 {
		t.Fatalf("summary is %d lines, want <= 60", len(lines))
	}
	for _, want := range []string{"# Migration devnet run", "straddle-fixture", "pre1", "node-1", "PASS C1", "INCONCLUSIVE C3", "FAIL C10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
}

// --- C6: mismatch and outside-window counting --------------------------

func sampleEvent(tm int64, roots map[string]string) migmon.Event {
	e := ev(migmon.EvSample, "", tm)
	e.Roots = roots
	return e
}

// Differing non-null roots in one sample are NOT a mismatch by
// themselves: partitions put nodes on different canonical chains at the
// sampled height, and the monitor - which groups by canonical hash -
// emits hash-split warns for those (observed live). The F1 critical is the
// mismatch authority; a split-shaped sample must not fail C6.
func TestCheckC6SplitShapedSampleIsNotAMismatch(t *testing.T) {
	els := []el{{name: "el1"}, {name: "el2"}}
	var events []migmon.Event
	for i := range 55 {
		events = append(events, sampleEvent(int64(i), map[string]string{"el1": "0xroot", "el2": "0xroot"}))
	}
	events = append(events, sampleEvent(9999, map[string]string{"el1": "0xaaa", "el2": "0xbbb"}))

	v := &verifier{els: els, monitor: events, skipChaos: true}
	pass, evidence := v.checkC6(context.Background())
	if pass != verdictPass {
		t.Fatalf("split-shaped sample failed C6, want pass with F1 as the only mismatch authority: %s", evidence)
	}
}

func TestCheckC6CriticalF1NeverWaived(t *testing.T) {
	els := []el{{name: "el1"}, {name: "el2"}}
	var events []migmon.Event
	for i := range 55 {
		events = append(events, sampleEvent(int64(i)+20, map[string]string{"el1": "0xroot", "el2": "0xroot"}))
	}
	crit := ev(migmon.EvCritical, "el1", 10)
	crit.Finding = migmon.FindingRootMismatch
	events = append(events, crit)

	chaos := []migmon.Event{ev(migmon.EvIsolate, "el1", 5), ev(migmon.EvHeal, "el1", 15)} // covers t=10
	v := &verifier{els: els, monitor: events, chaos: chaos}
	pass, evidence := v.checkC6(context.Background())
	if pass == verdictPass {
		t.Fatalf("F1 critical must never be waived, got pass")
	}
	if !strings.Contains(evidence, "F1") {
		t.Fatalf("evidence %q missing F1 reason", evidence)
	}
}

func TestCheckC6OutsideWindowCounting(t *testing.T) {
	els := []el{{name: "el1"}, {name: "el2"}}
	window := []migmon.Event{ev(migmon.EvIsolate, "el1", 1000), ev(migmon.EvHeal, "el1", 2000)}
	// covered range with 30s slop is [970, 2030].

	buildSamples := func(insideCount, outsideCount int) []migmon.Event {
		var events []migmon.Event
		for range insideCount {
			events = append(events, sampleEvent(1500, map[string]string{"el1": "0xroot", "el2": "0xroot"}))
		}
		for i := range outsideCount {
			events = append(events, sampleEvent(5000+int64(i), map[string]string{"el1": "0xroot", "el2": "0xroot"}))
		}
		return events
	}

	t.Run("exactly ten outside passes", func(t *testing.T) {
		v := &verifier{els: els, monitor: buildSamples(40, 10), chaos: window}
		pass, evidence := v.checkC6(context.Background())
		if pass != verdictPass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("nine outside fails though total good meets floor", func(t *testing.T) {
		v := &verifier{els: els, monitor: buildSamples(41, 9), chaos: window}
		pass, evidence := v.checkC6(context.Background())
		if pass == verdictPass {
			t.Fatalf("want fail (only 9 outside), got pass")
		}
		if !strings.Contains(evidence, "outside chaos windows") {
			t.Fatalf("evidence %q missing outside-window reason", evidence)
		}
	})

	t.Run("slop boundary: from-30 covered, from-31 not; to+30 covered, to+31 not", func(t *testing.T) {
		w := chaosWindow{node: "el1", from: time.Unix(1000, 0), to: time.Unix(2000, 0), healed: true}
		const slop = 30 * time.Second
		cases := []struct {
			t    int64
			want bool
		}{
			{970, true}, {969, false}, {2030, true}, {2031, false},
		}
		for _, c := range cases {
			got := w.covers("el1", time.Unix(c.t, 0), slop)
			if got != c.want {
				t.Errorf("covers(t=%d) = %v, want %v", c.t, got, c.want)
			}
		}
	})
}

// --- C7: pins pending logic ---------------------------------------------

func blockJSON(hash, root string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":"0x0","hash":%q,"stateRoot":%q,"timestamp":"0x0"}`, hash, root))
}

func TestCheckC7(t *testing.T) {
	hash := "0x" + strings.Repeat("aa", 32)
	root := "0x" + strings.Repeat("bb", 32)
	otherHash := "0x" + strings.Repeat("cc", 32)

	t.Run("pending fails without smoke", func(t *testing.T) {
		v := &verifier{pins: map[string]string{"genesis_hash": "pending", "genesis_state_root": "pending"}}
		pass, evidence := v.checkC7(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "pending") {
			t.Fatalf("want pending failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("pending under smoke is a warning pass", func(t *testing.T) {
		v := &verifier{pins: map[string]string{"genesis_hash": "pending", "genesis_state_root": "pending"}, smoke: true}
		pass, evidence := v.checkC7(context.Background())
		if pass != verdictPass || !strings.Contains(evidence, "WARN") {
			t.Fatalf("want warn pass, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("missing keys fail", func(t *testing.T) {
		v := &verifier{pins: map[string]string{"genesis_hash": hash}}
		pass, _ := v.checkC7(context.Background())
		if pass == verdictPass {
			t.Fatal("want fail on missing genesis_state_root key")
		}
	})

	t.Run("matching genesis passes", func(t *testing.T) {
		els := []el{{name: "el1", url: "http://el1"}, {name: "el2", url: "http://el2"}}
		responses := map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByNumber", "0x0", false): blockJSON(hash, root),
			rpcKey("http://el2", "eth_getBlockByNumber", "0x0", false): blockJSON(hash, root),
		}
		v := &verifier{els: els, pins: map[string]string{"genesis_hash": hash, "genesis_state_root": root}, fetch: fakeFetcher(t, responses)}
		pass, evidence := v.checkC7(context.Background())
		if pass != verdictPass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("mismatched genesis hash fails", func(t *testing.T) {
		els := []el{{name: "el1", url: "http://el1"}}
		responses := map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByNumber", "0x0", false): blockJSON(otherHash, root),
		}
		v := &verifier{els: els, pins: map[string]string{"genesis_hash": hash, "genesis_state_root": root}, fetch: fakeFetcher(t, responses)}
		pass, evidence := v.checkC7(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "!=") {
			t.Fatalf("want mismatch failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("state root alone is a sufficient pin", func(t *testing.T) {
		// kurtosis stamps a fresh genesis timestamp per run, so the hash
		// is per-run by design; the alloc (state root) is the invariant.
		els := []el{{name: "el1", url: "http://el1"}}
		responses := map[string]json.RawMessage{
			rpcKey("http://el1", "eth_getBlockByNumber", "0x0", false): blockJSON(otherHash, root),
		}
		v := &verifier{els: els, pins: map[string]string{"genesis_state_root": root}, fetch: fakeFetcher(t, responses)}
		pass, evidence := v.checkC7(context.Background())
		if pass != verdictPass || !strings.Contains(evidence, "stateRoot") {
			t.Fatalf("want root-only pass, got pass=%v evidence=%q", pass, evidence)
		}
	})
}

// --- C8: digest split ----------------------------------------------------

func TestCheckC8(t *testing.T) {
	snapshot := strings.Repeat("a", 64)
	preimages := strings.Repeat("b", 64)
	digestLine := func(snap, pre string) string {
		return fmt.Sprintf("PBT_ARTIFACT_DIGESTS /data/execution/pbt-artifacts/snapshot=%s /data/execution/pbt-artifacts/preimages=%s\n", snap, pre)
	}

	// Node names carry the client type: the digest contract is looked up in
	// the migmon registry by that substring, and a node without a contract
	// is skipped, not judged.
	geth1, geth2 := "el-1-geth-lighthouse", "el-2-geth-lighthouse"

	t.Run("identical digests across nodes pass", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), "startup\n"+digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, geth2+".log"), "startup\n"+digestLine(snapshot, preimages))
		v := &verifier{els: []el{{name: geth1}, {name: geth2}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass != verdictPass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("differing preimages digest fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, geth2+".log"), digestLine(snapshot, strings.Repeat("c", 64)))
		v := &verifier{els: []el{{name: geth1}, {name: geth2}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "differ") {
			t.Fatalf("want digest-differ failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("zero digest lines fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), "no digest here\n")
		v := &verifier{els: []el{{name: geth1}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "found 0") {
			t.Fatalf("want zero-line failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("two digest lines in one file fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages)+digestLine(snapshot, preimages))
		v := &verifier{els: []el{{name: geth1}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "found 2") {
			t.Fatalf("want two-line failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("a client with no digest contract is skipped, not judged", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, "el-2-unknownclient-lighthouse.log"), "no digest, and none required\n")
		v := &verifier{els: []el{{name: geth1}, {name: "el-2-unknownclient-lighthouse"}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass != verdictPass || !strings.Contains(evidence, "no digest contract") {
			t.Fatalf("want pass with a skip note, got pass=%v evidence=%q", pass, evidence)
		}
	})
}

// --- b* backwalk: binary search sanity -----------------------------------

func TestBackwalkBStar(t *testing.T) {
	// A 100-block chain where timestamp == number*10; T=505 falls strictly
	// between block 50 (500) and block 51 (510), so b*=51.
	responses := map[string]json.RawMessage{
		rpcKey("http://el1", "eth_getBlockByNumber", "latest", false): json.RawMessage(`{"number":"0x64","hash":"0x0000000000000000000000000000000000000000000000000000000000000000","stateRoot":"0x0000000000000000000000000000000000000000000000000000000000000000","timestamp":"0x3e8"}`),
	}
	fetch := func(ctx context.Context, url, method string, out any, params ...any) error {
		if method == "eth_getBlockByNumber" && params[0] != "latest" {
			n := parseHexUintForTest(t, params[0].(string))
			raw := json.RawMessage(fmt.Sprintf(`{"number":%q,"hash":"0x0000000000000000000000000000000000000000000000000000000000000000","stateRoot":"0x0000000000000000000000000000000000000000000000000000000000000000","timestamp":%q}`,
				fmt.Sprintf("0x%x", n), fmt.Sprintf("0x%x", n*10)))
			return json.Unmarshal(raw, out)
		}
		raw, ok := responses[rpcKey(url, method, params...)]
		if !ok {
			t.Fatalf("no stub for %s", rpcKey(url, method, params...))
		}
		return json.Unmarshal(raw, out)
	}
	v := &verifier{T: 505, fetch: fetch}
	got, err := v.backwalkBStar(context.Background(), el{name: "el1", url: "http://el1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 51 {
		t.Fatalf("b*=%d, want 51", got)
	}
}

func parseHexUintForTest(t *testing.T, s string) uint64 {
	t.Helper()
	var n uint64
	if _, err := fmt.Sscanf(s, "0x%x", &n); err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return n
}
