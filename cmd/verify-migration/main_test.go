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

// --- C3: healed-isolation waiver math ---------------------------------

func TestCheckC3(t *testing.T) {
	const T = 100000 // deadline = T-300 = 99700

	baseChaos := func() []migmon.Event {
		return []migmon.Event{
			ev(migmon.EvIsolate, "el1", 1000), ev(migmon.EvHeal, "el1", 1100),
			ev(migmon.EvIsolate, "el2", 2000), ev(migmon.EvHeal, "el2", 2100),
			ev(migmon.EvIsolate, "el3", 3000), ev(migmon.EvHeal, "el3", 3100),
			ev(migmon.EvIsolate, "el1", 4000), ev(migmon.EvHeal, "el1", 4100),
		}
	}
	reorg := func(node string, tm int64, depth int) migmon.Event {
		e := ev(migmon.EvReorg, node, tm)
		e.Detail = fmt.Sprintf("0xold -> 0xnew (depth %d)", depth)
		return e
	}
	baseReorgs := func() []migmon.Event {
		return []migmon.Event{
			reorg("el1", 1050, 5),
			reorg("el2", 2050, 12), // the >=10 one
			reorg("el3", 3050, 3),
			reorg("el1", 4050, 2),
		}
	}

	t.Run("four healed, one deep, all corroborated", func(t *testing.T) {
		v := &verifier{T: T, chaos: baseChaos(), monitor: baseReorgs()}
		pass, evidence := v.checkC3(context.Background())
		if !pass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("only three healed", func(t *testing.T) {
		chaos := baseChaos()
		chaos = chaos[:len(chaos)-1] // drop the closing heal for el1's second isolation
		v := &verifier{T: T, chaos: chaos, monitor: baseReorgs()}
		pass, evidence := v.checkC3(context.Background())
		if pass {
			t.Fatalf("want fail (only 3 healed), got pass")
		}
		if !strings.Contains(evidence, "only 3") {
			t.Fatalf("evidence %q missing healed-count reason", evidence)
		}
	})

	t.Run("no isolation reaches depth 10", func(t *testing.T) {
		reorgs := baseReorgs()
		reorgs[1] = reorg("el2", 2050, 9)
		v := &verifier{T: T, chaos: baseChaos(), monitor: reorgs}
		pass, evidence := v.checkC3(context.Background())
		if pass {
			t.Fatalf("want fail (no depth>=10), got pass")
		}
		if !strings.Contains(evidence, ">= 10") {
			t.Fatalf("evidence %q missing depth reason", evidence)
		}
	})

	t.Run("heal after T-300 deadline", func(t *testing.T) {
		chaos := baseChaos()
		chaos[3] = ev(migmon.EvHeal, "el2", 99750) // past deadline 99700
		v := &verifier{T: T, chaos: chaos, monitor: baseReorgs()}
		pass, evidence := v.checkC3(context.Background())
		if pass {
			t.Fatalf("want fail (late heal), got pass")
		}
		if !strings.Contains(evidence, "deadline") {
			t.Fatalf("evidence %q missing deadline reason", evidence)
		}
	})

	t.Run("under four matched isolations fails", func(t *testing.T) {
		reorgs := baseReorgs()
		reorgs = append(reorgs[:2], reorgs[3]) // drop el3's reorg entirely
		v := &verifier{T: T, chaos: baseChaos(), monitor: reorgs}
		pass, evidence := v.checkC3(context.Background())
		if pass {
			t.Fatalf("want fail (3 matched < 4), got pass")
		}
		if !strings.Contains(evidence, "matched by a reorg") || !strings.Contains(evidence, "el3") {
			t.Fatalf("evidence %q missing match-count reason naming the unmatched victim", evidence)
		}
	})

	t.Run("geth drop line accepted as a match with depth", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el3.log"),
			"some line\nINFO Chain reorg detected                     number=30 hash=6e845d..6e71d0 drop=3 dropfrom=3dc2e1..8b760b add=4\nother line\n")
		reorgs := append(baseReorgs()[:2], baseReorgs()[3]) // drop el3's monitor reorg, keep el2's depth-12 one
		v := &verifier{T: T, chaos: baseChaos(), monitor: reorgs, logsDir: dir}
		pass, evidence := v.checkC3(context.Background())
		if !pass {
			t.Fatalf("want pass via geth drop line, got fail: %s", evidence)
		}
	})

	t.Run("chaos node-N names match el-N log files and monitor events", func(t *testing.T) {
		// The chaos driver speaks participant indices ("node-2"); kurtosis
		// services and the monitor speak "el-2-geth-lighthouse". A live
		// run failed this check on exactly that gap.
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el-2-geth-lighthouse.log"),
			"INFO Chain reorg detected                     number=39 hash=6e845d..6e71d0 drop=11 dropfrom=3dc2e1..8b760b add=12\n")
		chaos := []migmon.Event{
			ev(migmon.EvIsolate, "node-2", 1000), ev(migmon.EvHeal, "node-2", 1100),
			ev(migmon.EvIsolate, "node-2", 2000), ev(migmon.EvHeal, "node-2", 2100),
			ev(migmon.EvIsolate, "node-3", 3000), ev(migmon.EvHeal, "node-3", 3100),
			ev(migmon.EvIsolate, "node-4", 4000), ev(migmon.EvHeal, "node-4", 4100),
		}
		monitor := []migmon.Event{
			reorg("el-2-geth-lighthouse", 2050, 4),
			reorg("el-3-geth-lighthouse", 3050, 2),
			reorg("el-4-geth-lighthouse", 4050, 1),
		}
		v := &verifier{T: T, chaos: chaos, monitor: monitor, logsDir: dir}
		pass, evidence := v.checkC3(context.Background())
		if !pass {
			t.Fatalf("want pass across naming conventions (log drop=11 covers depth>=10), got: %s", evidence)
		}
	})

	t.Run("skip-chaos short-circuits to pass", func(t *testing.T) {
		v := &verifier{T: T, skipChaos: true}
		pass, evidence := v.checkC3(context.Background())
		if !pass || !strings.Contains(evidence, "skipped") {
			t.Fatalf("want skipped pass, got pass=%v evidence=%q", pass, evidence)
		}
	})
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
	if !pass {
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
	if pass {
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
		if !pass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("nine outside fails though total good meets floor", func(t *testing.T) {
		v := &verifier{els: els, monitor: buildSamples(41, 9), chaos: window}
		pass, evidence := v.checkC6(context.Background())
		if pass {
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
		if pass || !strings.Contains(evidence, "pending") {
			t.Fatalf("want pending failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("pending under smoke is a warning pass", func(t *testing.T) {
		v := &verifier{pins: map[string]string{"genesis_hash": "pending", "genesis_state_root": "pending"}, smoke: true}
		pass, evidence := v.checkC7(context.Background())
		if !pass || !strings.Contains(evidence, "WARN") {
			t.Fatalf("want warn pass, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("missing keys fail", func(t *testing.T) {
		v := &verifier{pins: map[string]string{"genesis_hash": hash}}
		pass, _ := v.checkC7(context.Background())
		if pass {
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
		if !pass {
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
		if pass || !strings.Contains(evidence, "!=") {
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
		if !pass || !strings.Contains(evidence, "stateRoot") {
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

	t.Run("identical digests across nodes pass", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el1.log"), "startup\n"+digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, "el2.log"), "startup\n"+digestLine(snapshot, preimages))
		v := &verifier{els: []el{{name: "el1"}, {name: "el2"}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if !pass {
			t.Fatalf("want pass, got fail: %s", evidence)
		}
	})

	t.Run("differing preimages digest fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el1.log"), digestLine(snapshot, preimages))
		writeFile(t, filepath.Join(dir, "el2.log"), digestLine(snapshot, strings.Repeat("c", 64)))
		v := &verifier{els: []el{{name: "el1"}, {name: "el2"}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass || !strings.Contains(evidence, "differ") {
			t.Fatalf("want digest-differ failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("zero digest lines fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el1.log"), "no digest here\n")
		v := &verifier{els: []el{{name: "el1"}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass || !strings.Contains(evidence, "found 0") {
			t.Fatalf("want zero-line failure, got pass=%v evidence=%q", pass, evidence)
		}
	})

	t.Run("two digest lines in one file fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "el1.log"), digestLine(snapshot, preimages)+digestLine(snapshot, preimages))
		v := &verifier{els: []el{{name: "el1"}}, logsDir: dir}
		pass, evidence := v.checkC8(context.Background())
		if pass || !strings.Contains(evidence, "found 2") {
			t.Fatalf("want two-line failure, got pass=%v evidence=%q", pass, evidence)
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
