package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// --- requireOffline: every offline check routes through this -----------------

func TestRequireOffline(t *testing.T) {
	cases := []struct {
		name string
		v    *verifier
		want verdict
	}{
		{"no --manifest given", &verifier{}, verdictInconclusive},
		{"manifest unreadable", &verifier{manifestPath: "x", manifestErr: errors.New("boom")}, verdictInconclusive},
		{"manifest has no offline block", &verifier{manifestPath: "x", manifest: &lapManifest{}}, verdictInconclusive},
		{"offline present but disabled", &verifier{manifestPath: "x", manifest: &lapManifest{Offline: &migmon.Offline{Enabled: false}}}, verdictInconclusive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, r, msg := c.v.requireOffline()
			if r != c.want {
				t.Fatalf("want %s, got %s: %s", c.want, r, msg)
			}
		})
	}
}

// --- replay-caught-up corroboration ----------------------------------------

// Caught up means within CaughtUpLag of the chain tip (node 1's head), not of the
// consumer's own head, which right after its restart is as stale as its cursor
// (2026-10-05 lap 4: Nethermind at cursor = head 222 while the tip was 229).
func TestLifecycleCaughtUpMeasuresAgainstTheTip(t *testing.T) {
	at := func(kind, node string, sec int64, n uint64, cursor uint64) migmon.Event {
		e := ev(kind, node, sec)
		e.Number = n
		if kind == migmon.EvProgress {
			raw, err := json.Marshal(migmon.MigrationProgress{Phase: migmon.PhaseRunning,
				Binary: &migmon.DirectionProgress{Phase: migmon.DirSynced, Cursor: migmon.FlexUint64(cursor)}})
			if err != nil {
				t.Fatal(err)
			}
			e.Raw = raw
		}
		return e
	}
	const tipNode, consumer = "el-1-geth-lighthouse", "el-5-nethermind-lighthouse"
	stale := []migmon.Event{
		at(migmon.EvHead, tipNode, 100, 229, 0),
		at(migmon.EvHead, consumer, 101, 222, 0),
		at(migmon.EvProgress, consumer, 101, 0, 222),
	}
	if (&verifier{monitor: stale}).lifecycleCaughtUp(5, time.Unix(50, 0)) {
		t.Fatal("cursor at the consumer's own stale head, 7 behind the tip, corroborated caught_up")
	}
	synced := append(stale, at(migmon.EvProgress, consumer, 110, 0, 227))
	if !(&verifier{monitor: synced}).lifecycleCaughtUp(5, time.Unix(50, 0)) {
		t.Fatal("cursor within CaughtUpLag of the tip did not corroborate caught_up")
	}
}

// --- swaps-serialized -------------------------------------------------------

func TestCheckSwapsSerialized(t *testing.T) {
	dump := migsched.Dump{Profile: "p", Fork: testFork, Anchor: 1, Ops: []migsched.DumpOp{
		{Name: "op1", Class: "short", Start: 5000, End: 5100, Victims: []int{4}},
	}}
	offline := func() *migmon.Offline {
		return &migmon.Offline{Enabled: true, Consumers: []migmon.OfflineConsumer{
			{Node: 5, TimeoutSeconds: 900}, {Node: 3, TimeoutSeconds: 900},
		}}
	}
	run := func(t *testing.T, swaps []migmon.SwapRecord) (verdict, string) {
		t.Helper()
		v := &verifier{
			manifestPath: "x",
			manifest:     &lapManifest{Offline: offline(), Swaps: swaps},
			chaos:        []migmon.Event{scheduleEvent(t, dump)},
		}
		return v.checkSwapsSerialized(context.Background())
	}

	t.Run("ok: two serialized, chaos-free, within budget", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, StopAt: 1000, StartAt: 1010, CaughtUpAt: 1200},
			{Node: 3, StopAt: 2000, StartAt: 2010, CaughtUpAt: 2200},
		})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("ok: back-to-back successful swaps don't double-pad into a false overlap", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, StopAt: 1000, CaughtUpAt: 1200},
			{Node: 3, StopAt: 1210, CaughtUpAt: 1300},
		})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("overlap: two consumer windows intersect", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, StopAt: 1000, StartAt: 1010, CaughtUpAt: 1200},
			{Node: 3, StopAt: 1100, StartAt: 1110, CaughtUpAt: 1300},
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "overlap") {
			t.Fatalf("evidence %q missing overlap reason", evidence)
		}
	})

	t.Run("node 1 targeted: fails", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 1, StopAt: 1000, StartAt: 1010, CaughtUpAt: 1200},
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "node 1") {
			t.Fatalf("evidence %q missing node-1 reason", evidence)
		}
	})

	t.Run("inside an admitted op: fails", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, StopAt: 5000, StartAt: 5010, CaughtUpAt: 5050},
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "admitted op") {
			t.Fatalf("evidence %q missing admitted-op reason", evidence)
		}
	})

	t.Run("over budget: caught up past its timeout", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, StopAt: 1000, StartAt: 1010, CaughtUpAt: 3000},
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "timeout") {
			t.Fatalf("evidence %q missing timeout reason", evidence)
		}
	})

	// The 2026-10-05 lap 3 shape: node 5 timed out, so node 3 was skipped as too late
	// and never stopped. That is no serialization fault.
	t.Run("skipped swap never stopped: not a serialization fault", func(t *testing.T) {
		r, evidence := run(t, []migmon.SwapRecord{
			{Node: 5, Status: migmon.StepTimeout, StopAt: 1000, StartAt: 1010},
			{Node: 3, Status: migmon.StepSkipped, Detail: "too late"},
		})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})
}

// --- import-accepted ---------------------------------------------------------

func TestCheckImportAccepted(t *testing.T) {
	root := "0x" + strings.Repeat("5", 64)

	run := func(t *testing.T, elName string, swap migmon.SwapRecord, files map[string]string) (verdict, string) {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			writeFile(t, filepath.Join(dir, swap.EvidenceDir, name), body)
		}
		v := &verifier{
			manifestPath: "x", artifactsDir: dir,
			monitor:  []migmon.Event{ev(migmon.EvHead, elName, 0)},
			manifest: &lapManifest{Offline: &migmon.Offline{Enabled: true}, Swaps: []migmon.SwapRecord{swap}},
		}
		return v.checkImportAccepted(context.Background())
	}

	t.Run("geth: clean import accepted", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 3, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-3"}
		r, evidence := run(t, "el-3-geth-lighthouse", swap, map[string]string{
			"import.log": "INFO Import complete binaryRoot=" + root + " anchor=812\n",
			"done":       "",
		})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("geth: failed marker present: fails", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 3, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-3"}
		r, evidence := run(t, "el-3-geth-lighthouse", swap, map[string]string{
			"import.log": "INFO Import complete binaryRoot=" + root + " anchor=812\n",
			"failed":     "",
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "failed marker") {
			t.Fatalf("evidence %q missing failed-marker reason", evidence)
		}
	})

	t.Run("geth: import.log anchor mismatch: fails", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 3, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-3"}
		r, evidence := run(t, "el-3-geth-lighthouse", swap, map[string]string{
			"import.log": "INFO Import complete binaryRoot=" + root + " anchor=999\n",
			"done":       "",
		})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "!= manifest anchor") {
			t.Fatalf("evidence %q missing anchor-mismatch reason", evidence)
		}
	})

	t.Run("swap skipped: fails with its detail, no log reading", func(t *testing.T) {
		// No files given: if the check tried to read a log anyway it would fail with
		// an "open ...: no such file" error instead of this detail.
		swap := migmon.SwapRecord{Node: 5, Status: migmon.StepSkipped, Detail: "producer 6 failed", EvidenceDir: "consumer-node-5"}
		r, evidence := run(t, "el-5-nethermind-lighthouse", swap, nil)
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "swap skipped: producer 6 failed") {
			t.Fatalf("evidence %q missing the skip detail", evidence)
		}
	})

	t.Run("nethermind: refusal line logged: fails", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 5, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-5"}
		r, evidence := run(t, "el-5-nethermind-lighthouse", swap, map[string]string{"docker.log": "pbt-swap: starting with --Pbt.MigrationAnchor=812\n" +
			"Migration anchor is not present in the trusted canonical chain\n"})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "refusal logged") {
			t.Fatalf("evidence %q missing refusal reason", evidence)
		}
	})

	t.Run("nethermind: no boot marker: fails", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 5, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-5"}
		r, evidence := run(t, "el-5-nethermind-lighthouse", swap, map[string]string{"docker.log": "EIP-8347 migration: flat state at StateId { BlockNumber = 0, StateRoot = 0x1a }, PBT state at StateId { BlockNumber = 0, StateRoot = 0x1a }.\n"})
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "no swap boot to judge") {
			t.Fatalf("evidence %q missing no-boot-marker reason", evidence)
		}
	})

	t.Run("nethermind: the swap boot's coloured success line is judged, not the genesis boot's", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 5, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-5"}
		// The shape a real lap leaves in docker.log: nethermind colours the line, and the
		// container's first boot (seeded from genesis) logged its own success at block 0.
		log := "EIP-8347 migration: flat state at StateId { BlockNumber = 0, StateRoot = 0x1a\x1b[97m }\x1b[0m, PBT state at StateId { BlockNumber = 0, StateRoot = 0x1a\x1b[97m }.\x1b[0m\n" +
			"pbt-swap: starting with --Pbt.MigrationAnchor=812\n" +
			"EIP-8347 migration: flat state at StateId { BlockNumber = 100, StateRoot = 0xaaa\x1b[97m }\x1b[0m, PBT state at StateId { BlockNumber = 812, StateRoot = " + root + "\x1b[97m }.\x1b[0m\n"
		r, evidence := run(t, "el-5-nethermind-lighthouse", swap, map[string]string{"docker.log": log})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("nethermind: a later reuse boot (restart after the swap) still passes", func(t *testing.T) {
		swap := migmon.SwapRecord{Node: 5, Anchor: 812, FinalizedAtStop: 812, EvidenceDir: "consumer-node-5"}
		// A plain restart after the swap boots again, logs the wrapper's marker a second
		// time, then reuses the already-imported PBT database instead of re-logging the
		// migration success line: LastIndex would anchor on this second marker and see no
		// success line at all. The first marker keeps the swap boot's own success line.
		log := "EIP-8347 migration: flat state at StateId { BlockNumber = 0, StateRoot = 0x1a }, PBT state at StateId { BlockNumber = 0, StateRoot = 0x1a }.\n" +
			"pbt-swap: starting with --Pbt.MigrationAnchor=812\n" +
			"EIP-8347 migration: flat state at StateId { BlockNumber = 100, StateRoot = 0xaaa }, PBT state at StateId { BlockNumber = 812, StateRoot = " + root + " }.\n" +
			"pbt-swap: starting with --Pbt.MigrationAnchor=812\n" +
			"native PBT database was imported earlier; reusing it\n" +
			"VerifyAlignment: head block 900\n"
		r, evidence := run(t, "el-5-nethermind-lighthouse", swap, map[string]string{"docker.log": log})
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})
}

// --- artifact-produced --------------------------------------------------------

func TestCheckArtifactProduced(t *testing.T) {
	const anchor = uint64(812)
	anchorHash := "0x" + strings.Repeat("a", 64)
	anchorStateRoot := "0x" + strings.Repeat("9", 64)
	genesisRoot := "0x" + strings.Repeat("0", 64)
	trailerRoot := common.HexToHash("0x" + strings.Repeat("5", 64))
	rootHex := trailerRoot.Hex()
	otherRoot := common.HexToHash("0x" + strings.Repeat("6", 64)).Hex()

	els := []el{{name: "el-6-geth-lighthouse", url: "http://el6"}}
	fetch := fakeFetcher(t, map[string]json.RawMessage{
		rpcKey("http://el6", "eth_getBlockByNumber", hexutil.EncodeUint64(anchor), false): blockJSON(anchorHash, anchorStateRoot),
	})

	run := func(t *testing.T, shadowRoots map[string]string, snapshotTrailer common.Hash) (verdict, string) {
		t.Helper()
		dir := t.TempDir()
		data := append([]byte{0x07}, snapshotTrailer.Bytes()...)
		writeFile(t, filepath.Join(dir, "node-6", "pbt-snapshot.bin"), string(data))
		writeFile(t, filepath.Join(dir, "node-6", "convert.log"),
			"Starting MPT to binary trie conversion block=812\nConversion complete binaryRoot="+rootHex+"\n")
		producer := migmon.ProducerRecord{
			Node: 6, Kind: "geth-convert", Status: "ok",
			Anchor: anchor, AnchorHash: anchorHash, AnchorStateRoot: anchorStateRoot, PbtRoot: rootHex,
			ShadowRoots: shadowRoots, Dir: "node-6",
		}
		v := &verifier{
			manifestPath: "x", artifactsDir: dir, els: els, fetch: fetch, T: 1000,
			pins:     map[string]string{"genesis_state_root": genesisRoot},
			manifest: &lapManifest{Offline: &migmon.Offline{Enabled: true}, Producers: []migmon.ProducerRecord{producer}},
		}
		return v.checkArtifactProduced(context.Background())
	}

	agreeing := map[string]string{"el-1-geth-lighthouse": rootHex, "el-5-nethermind-lighthouse": rootHex}

	t.Run("ok: trailer matches, shadow roots agree across 2 clients", func(t *testing.T) {
		r, evidence := run(t, agreeing, trailerRoot)
		if r != verdictPass {
			t.Fatalf("want pass, got %s: %s", r, evidence)
		}
	})

	t.Run("snapshot trailer mismatch: fails", func(t *testing.T) {
		r, evidence := run(t, agreeing, common.HexToHash(otherRoot))
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "snapshot trailer") {
			t.Fatalf("evidence %q missing trailer reason", evidence)
		}
	})

	t.Run("shadow roots dissent: fails", func(t *testing.T) {
		dissenting := map[string]string{"el-1-geth-lighthouse": rootHex, "el-5-nethermind-lighthouse": otherRoot}
		r, evidence := run(t, dissenting, trailerRoot)
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "!= pbt_root") {
			t.Fatalf("evidence %q missing dissent reason", evidence)
		}
	})

	t.Run("shadow roots too few distinct clients: fails", func(t *testing.T) {
		tooFew := map[string]string{"el-1-geth-lighthouse": rootHex, "el-4-besu-lighthouse": ""}
		r, evidence := run(t, tooFew, trailerRoot)
		if r != verdictFail {
			t.Fatalf("want fail, got %s: %s", r, evidence)
		}
		if !strings.Contains(evidence, "distinct client") {
			t.Fatalf("evidence %q missing too-few reason", evidence)
		}
	})
}

// --- waivers extend to offline swap/producer windows -------------------------

func TestWaiverWindowsCoverSwapAndProducerDowntime(t *testing.T) {
	sample := ev(migmon.EvSample, "", 1150)
	warn := ev(migmon.EvWarn, "el-5-nethermind-lighthouse", 1150)
	warn.Finding = migmon.FindingRootMismatch

	t.Run("no offline manifest: unwaived, fails", func(t *testing.T) {
		v := &verifier{monitor: []migmon.Event{sample, warn}}
		r, evidence := v.checkPostForkSamples(context.Background())
		if r != verdictFail {
			t.Fatalf("want fail (nothing to waive it), got %s: %s", r, evidence)
		}
	})

	t.Run("root-mismatch inside the consumer's swap window: waived, passes", func(t *testing.T) {
		manifest := &lapManifest{
			Offline: &migmon.Offline{Enabled: true},
			Swaps:   []migmon.SwapRecord{{Node: 5, StopAt: 1000, CaughtUpAt: 1200}},
		}
		v := &verifier{monitor: []migmon.Event{sample, warn}, manifestPath: "x", manifest: manifest}
		r, evidence := v.checkPostForkSamples(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (waived by node 5's swap window), got %s: %s", r, evidence)
		}
	})

	t.Run("root-mismatch inside the producer's downtime window: waived, passes", func(t *testing.T) {
		manifest := &lapManifest{
			Offline:   &migmon.Offline{Enabled: true},
			Producers: []migmon.ProducerRecord{{Node: 5, DownFrom: 1000, DownTo: 1200}},
		}
		v := &verifier{monitor: []migmon.Event{sample, warn}, manifestPath: "x", manifest: manifest}
		r, evidence := v.checkPostForkSamples(context.Background())
		if r != verdictPass {
			t.Fatalf("want pass (waived by node 5's producer downtime window), got %s: %s", r, evidence)
		}
	})
}
