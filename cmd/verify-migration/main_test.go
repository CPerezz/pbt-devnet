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

// --- schedule-driven fixtures shared by partitions-healed/prefork-deep-reorg/straddle-rewind -----------------------

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
// partitions-healed/prefork-deep-reorg variant below tweaks from. Indices: 0/1 = pre1 isolate/heal,
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

// --- partitions-healed: schedule-derived expectations and per-class deadlines ----------

func TestCheckPartitionsHealedScheduleDrivenDeadlines(t *testing.T) {
	// Quiet is what tells a post-migration isolation from a stray one, and
	// the driver always publishes it.
	dump := migsched.Dump{Profile: "straddle-fixture", Fork: testFork, Heavy: 4, Ops: straddleTestOps(), Quiet: testFork + 210}

	t.Run("baseline: all four heal on time, fully corroborated, passes", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (straddle's deadline is End+120s, not the fork), got %s: %s", r, evidence)
		}
	})

	t.Run("pre-fork op heals after its own deadline (the fork itself): fails", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		chaos[1].Time = time.Unix(testFork+10, 0).UTC() // pre1 heal, past the fork
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
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
		r, evidence := v.checkPartitionsHealed(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive (thin coverage), got %s: %s", r, evidence)
		}
	})

	t.Run("missing schedule record fails: the run's own schedule is mandatory input", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		v := &verifier{T: uint64(testFork), chaos: chaos, monitor: monitor} // no schedule event
		r, evidence := v.checkPartitionsHealed(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (no schedule record), got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "schedule") {
			t.Fatalf("evidence %q missing schedule reason", evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass even with no schedule", func(t *testing.T) {
		v := &verifier{T: uint64(testFork), skipChaos: true}
		r, evidence := v.checkPartitionsHealed(context.Background())
		if r != verdictPass || !strings.Contains(evidence, "skipped") {
			t.Fatalf("want skipped pass, got %s evidence=%q", r, evidence)
		}
	})
}

// --- prefork-deep-reorg: deep reorg, split out of partitions-healed's pass/fail -------------------------

func TestCheckPreForkDeepReorgDeepReorg(t *testing.T) {
	dump := migsched.Dump{Profile: "p", Fork: testFork, Ops: straddleTestOps()}

	t.Run("no isolation reaches depth 10: inconclusive, not fail", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor() // max depth 8
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkPreForkDeepReorg(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("one isolation reaches depth >= 10: passes", func(t *testing.T) {
		chaos, monitor := baselineChaosAndMonitor()
		monitor[2] = reorgEv("node-3", 3060, 12)
		v := &verifier{T: uint64(testFork), chaos: withSchedule(t, dump, chaos), monitor: monitor}
		r, evidence := v.checkPreForkDeepReorg(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "pre3") {
			t.Fatalf("evidence %q should name the deep op", evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass", func(t *testing.T) {
		v := &verifier{skipChaos: true}
		r, _ := v.checkPreForkDeepReorg(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s", r)
		}
	})
}

// --- straddle-rewind: the straddle actually happened ---------------------------------

var (
	testOldHash = "0x" + strings.Repeat("a", 64)
	testNewHash = "0x" + strings.Repeat("b", 64)
	zeroRoot    = "0x" + strings.Repeat("0", 64)
)

func istarReorgedEv(node string, height uint64, oldHash, newHash string, tm int64) migmon.Event {
	e := ev(migmon.EvIStarReorged, node, tm)
	e.Number = height
	e.Hash = newHash
	e.Detail = fmt.Sprintf("fork block %d %s was orphaned; height %d now holds %s", height, oldHash, height, newHash)
	return e
}

func TestCheckStraddleRewindStraddle(t *testing.T) {
	straddleOp := migsched.DumpOp{Name: "straddle1", Class: "straddle", Start: testFork - 50, End: testFork + 50, Victims: []int{4}}
	nonStraddleOnly := migsched.Dump{Profile: "p", Fork: testFork, Ops: straddleTestOps()[:1]}
	withStraddle := migsched.Dump{Profile: "p", Fork: testFork, Ops: []migsched.DumpOp{straddleOp}}

	t.Run("no straddle admitted: inconclusive", func(t *testing.T) {
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, nonStraddleOnly)}}
		r, evidence := v.checkStraddleRewind(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted but victim never orphaned: inconclusive", func(t *testing.T) {
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}}
		r, evidence := v.checkStraddleRewind(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted, victim orphaned, dropped branch >= 6: passes", func(t *testing.T) {
		monitor := []migmon.Event{
			istarReorgedEv("node-4", 500, testOldHash, testNewHash, testFork+10),
			reorgEv("node-4", testFork+15, 8),
		}
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}, monitor: monitor}
		r, evidence := v.checkStraddleRewind(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("straddle admitted, victim orphaned, dropped branch < 6: fails", func(t *testing.T) {
		monitor := []migmon.Event{
			istarReorgedEv("node-4", 500, testOldHash, testNewHash, testFork+10),
			reorgEv("node-4", testFork+15, 3),
		}
		v := &verifier{chaos: []migmon.Event{scheduleEvent(t, withStraddle)}, monitor: monitor}
		r, evidence := v.checkStraddleRewind(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass", func(t *testing.T) {
		v := &verifier{skipChaos: true}
		r, _ := v.checkStraddleRewind(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass, got %s", r)
		}
	})
}

// --- forkblock-convergence: boundary convergence -------------------------------------------

func TestCheckForkBlockConvergenceBoundaryConvergence(t *testing.T) {
	hashA := "0x" + strings.Repeat("a", 64)
	hashB := "0x" + strings.Repeat("b", 64)
	hashC := "0x" + strings.Repeat("c", 64)
	els := []el{{name: "el1", url: "http://el1"}, {name: "el2", url: "http://el2"}}

	finalRecords := []migmon.Event{
		{Kind: migmon.EvIStarFinal, Node: "el1", Number: 500, Hash: hashA, Time: time.Unix(1000, 0)},
		{Kind: migmon.EvIStarFinal, Node: "el2", Number: 500, Hash: hashA, Time: time.Unix(1000, 0)},
	}
	provisionalDiversity := []migmon.Event{
		{Kind: migmon.EvIStar, Node: "el1", Hash: hashB, Time: time.Unix(500, 0)},
		{Kind: migmon.EvIStar, Node: "el2", Hash: hashC, Time: time.Unix(500, 0)},
	}
	agreeingFetch := fakeFetcher(t, map[string]json.RawMessage{
		rpcKey("http://el1", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashA, zeroRoot),
		rpcKey("http://el2", "eth_getBlockByNumber", "0x1f4", false): blockJSON(hashA, zeroRoot),
	})

	t.Run("agreement, no no-convergence, >=2 provisional hashes: passes", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		v := &verifier{els: els, monitor: monitor, fetch: agreeingFetch}
		r, evidence := v.checkForkBlockConvergence(context.Background())
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
		r, evidence := v.checkForkBlockConvergence(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})

	t.Run("unwaived no-convergence critical: fails", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		crit := ev(migmon.EvCritical, "el1", 99999)
		crit.Finding = migmon.FindingNoConvergence
		monitor = append(monitor, crit)
		v := &verifier{els: els, monitor: monitor, fetch: agreeingFetch}
		r, evidence := v.checkForkBlockConvergence(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "no-convergence") {
			t.Fatalf("evidence %q missing no-convergence reason", evidence)
		}
	})

	t.Run("no-convergence waived inside a chaos window does not fail on that alone", func(t *testing.T) {
		monitor := append(append([]migmon.Event{}, finalRecords...), provisionalDiversity...)
		crit := ev(migmon.EvCritical, "el1", 1500)
		crit.Finding = migmon.FindingNoConvergence
		monitor = append(monitor, crit)
		chaos := []migmon.Event{ev(migmon.EvIsolate, "el1", 1400), ev(migmon.EvHeal, "el1", 1600)}
		v := &verifier{els: els, monitor: monitor, chaos: chaos, fetch: agreeingFetch}
		r, evidence := v.checkForkBlockConvergence(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (no-convergence waived by chaos window), got %s: %s", r, evidence)
		}
	})

	t.Run("fewer than 2 distinct provisional hashes: inconclusive", func(t *testing.T) {
		v := &verifier{els: els, monitor: finalRecords, fetch: agreeingFetch}
		r, evidence := v.checkForkBlockConvergence(context.Background())
		if r != verdictInconclusive {
			t.Fatalf("want inconclusive, got %s: %s", r, evidence)
		}
	})

	t.Run("no istar record observed at all: fails", func(t *testing.T) {
		v := &verifier{els: els}
		r, evidence := v.checkForkBlockConvergence(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
	})
}

// --- orphan-gone: the orphaned branch is actually gone ---------------------------

func TestCheckOrphanGoneOrphanedBranchGone(t *testing.T) {
	els := []el{{name: "el1", url: "http://el1"}, {name: "el2", url: "http://el2"}}
	orphanHash := testOldHash
	monitor := []migmon.Event{istarReorgedEv("node-4", 500, orphanHash, testNewHash, testFork+10)}

	t.Run("no orphaned block identified: inconclusive", func(t *testing.T) {
		v := &verifier{els: els}
		r, evidence := v.checkOrphanGone(context.Background())
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
		r, evidence := v.checkOrphanGone(context.Background())
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
		r, evidence := v.checkOrphanGone(context.Background())
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
		r, evidence := v.checkOrphanGone(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "canonical") {
			t.Fatalf("evidence %q missing canonical reason", evidence)
		}
	})
}

// --- per-client reorg log registry ---------------------------------------

func TestLogReorgMatchesExtractsDropAncestorAndTime(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "el-7-geth-lighthouse.log")
	writeFile(t, logPath, "INFO [09-07|08:56:31.211] Chain reorg detected number=39 hash=6e845d..6e71d0 drop=11 dropfrom=3dc2e1..8b760b add=12\n")
	ref := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	matches := logReorgMatches(logPath, clientLogPatterns["geth"], ref)
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(matches))
	}
	// number= is the common-ancestor height geth logs after walking both chains back.
	if matches[0].drop != 11 || matches[0].ancestor != 39 {
		t.Fatalf("want drop=11 ancestor=39, got %+v", matches[0])
	}
	if want := time.Date(2026, 9, 7, 8, 56, 31, 0, time.UTC); !matches[0].at.Equal(want) {
		t.Fatalf("want stamp %s, got %s", want, matches[0].at)
	}
}

// A reorg line logged outside the window is not evidence for that window: a
// stale deep-partition line must not vouch for a later straddle.
func TestMatchReorgIgnoresLogLinesOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "el-2-geth-lighthouse.log"),
		"INFO [09-07|08:30:00.000] Chain reorg detected number=78 drop=16 add=1\n"+
			"INFO [09-07|08:57:00.000] Chain reorg detected number=250 drop=9 add=1\n")
	from := time.Date(2026, 9, 7, 8, 53, 0, 0, time.UTC)
	w := chaosWindow{node: "node-2", from: from, to: from.Add(5 * time.Minute), healed: true}
	got := (&verifier{logsDir: dir}).matchReorg(w)
	if !got.matched || got.depth != 9 || got.ancestor != 250 {
		t.Fatalf("want the in-window drop=9 line, got %+v", got)
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

func sampleEvent(tm int64, roots map[string]string) migmon.Event {
	e := ev(migmon.EvSample, "", tm)
	e.Roots = roots
	return e
}

// Differing non-null roots in one sample are NOT a mismatch by
// themselves: partitions put nodes on different canonical chains at the
// sampled height, and the monitor - which groups by canonical hash -
// emits hash-split warns for those (observed live). The root-mismatch critical is the
// mismatch authority; a split-shaped sample must not fail shadow-samples.
func TestCheckShadowSamplesSplitShapedSampleIsNotAMismatch(t *testing.T) {
	els := []el{{name: "el1"}, {name: "el2"}}
	var events []migmon.Event
	for i := range 55 {
		events = append(events, sampleEvent(int64(i), map[string]string{"el1": "0xroot", "el2": "0xroot"}))
	}
	events = append(events, sampleEvent(9999, map[string]string{"el1": "0xaaa", "el2": "0xbbb"}))

	v := &verifier{els: els, monitor: events, skipChaos: true}
	pass, evidence := v.checkShadowSamples(context.Background())
	if pass != verdictPass {
		t.Fatalf("split-shaped sample failed shadow-samples, want pass with root-mismatch as the only mismatch authority: %s", evidence)
	}
}

func TestCheckShadowSamplesRootMismatchNeverWaived(t *testing.T) {
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
	pass, evidence := v.checkShadowSamples(context.Background())
	if pass == verdictPass {
		t.Fatalf("root-mismatch critical must never be waived, got pass")
	}
	if !strings.Contains(evidence, "root-mismatch") {
		t.Fatalf("evidence %q missing root-mismatch reason", evidence)
	}
}

func TestCheckShadowSamplesOutsideWindowCounting(t *testing.T) {
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
		pass, evidence := v.checkShadowSamples(context.Background())
		if pass != verdictPass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("nine outside fails though total good meets floor", func(t *testing.T) {
		v := &verifier{els: els, monitor: buildSamples(41, 9), chaos: window}
		pass, evidence := v.checkShadowSamples(context.Background())
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

// --- genesis-pins: pins pending logic ---------------------------------------------

func blockJSON(hash, root string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":"0x0","hash":%q,"stateRoot":%q,"timestamp":"0x0"}`, hash, root))
}

func TestCheckBootstrapDigests(t *testing.T) {
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
		pass, evidence := v.checkBootstrapDigests(context.Background())
		if pass != verdictPass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("differing preimages digest fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, geth2+".log"), digestLine(snapshot, strings.Repeat("c", 64)))
		v := &verifier{els: []el{{name: geth1}, {name: geth2}}, logsDir: dir}
		pass, evidence := v.checkBootstrapDigests(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "differ") {
			t.Fatalf("want digest-differ failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("zero digest lines fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), "no digest here\n")
		v := &verifier{els: []el{{name: geth1}}, logsDir: dir}
		pass, evidence := v.checkBootstrapDigests(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "found 0") {
			t.Fatalf("want zero-line failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("two digest lines in one file fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages)+digestLine(snapshot, preimages))
		v := &verifier{els: []el{{name: geth1}}, logsDir: dir}
		pass, evidence := v.checkBootstrapDigests(context.Background())
		if pass == verdictPass || !strings.Contains(evidence, "found 2") {
			t.Fatalf("want two-line failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("a client with no digest contract is skipped, not judged", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, geth1+".log"), digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, "el-2-unknownclient-lighthouse.log"), "no digest, and none required\n")
		v := &verifier{els: []el{{name: geth1}, {name: "el-2-unknownclient-lighthouse"}}, logsDir: dir}
		pass, evidence := v.checkBootstrapDigests(context.Background())
		if pass != verdictPass || !strings.Contains(evidence, "no digest contract") {
			t.Fatalf("want pass with a skip note, got pass=%v evidence=%q", pass, evidence)
		}
	})
}

// --- I* backwalk: binary search sanity -----------------------------------

func TestBackwalkIStar(t *testing.T) {
	// A 100-block chain where timestamp == number*10; T=505 falls strictly
	// between block 50 (500) and block 51 (510), so I*=51.
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
	got, err := v.backwalkIStar(context.Background(), el{name: "el1", url: "http://el1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 51 {
		t.Fatalf("I*=%d, want 51", got)
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
