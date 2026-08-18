package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// rpcBlock is the subset of eth_getBlockByNumber we compare on.
type rpcBlock struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	StateRoot  common.Hash    `json:"stateRoot"`
	Timestamp  hexutil.Uint64 `json:"timestamp"`
	GasLimit   hexutil.Uint64 `json:"gasLimit"`
	GasUsed    hexutil.Uint64 `json:"gasUsed"`
}

func getBlock(ctx context.Context, n *Node, numberOrTag string) (*rpcBlock, error) {
	var blk *rpcBlock
	if err := n.RPC(ctx, "eth_getBlockByNumber", &blk, numberOrTag, false); err != nil {
		return nil, err
	}
	if blk == nil {
		return nil, fmt.Errorf("%s has no block %s", n.Name, numberOrTag)
	}
	return blk, nil
}

func getBlockByHash(ctx context.Context, n *Node, hash common.Hash) (*rpcBlock, error) {
	var blk *rpcBlock
	if err := n.RPC(ctx, "eth_getBlockByHash", &blk, hash, false); err != nil {
		return nil, err
	}
	if blk == nil {
		return nil, fmt.Errorf("%s does not know block %s", n.Name, hash)
	}
	return blk, nil
}

// assertHeadsAgree is oracle 3: every node must sit on the same head, at the
// expected height, with the same state root.
func (d *Driver) assertHeadsAgree(ctx context.Context, want common.Hash, wantNum uint64) error {
	heads := make([]*rpcBlock, len(d.nodes))
	for i, n := range d.nodes {
		blk, err := getBlock(ctx, n, "latest")
		if err != nil {
			return err
		}
		heads[i] = blk
	}
	for i := 1; i < len(d.nodes); i++ {
		if heads[i].Hash != heads[0].Hash {
			return fmt.Errorf("head divergence: %s=%s@%d (root %s) vs %s=%s@%d (root %s)",
				d.nodes[0].Name, heads[0].Hash, heads[0].Number, heads[0].StateRoot,
				d.nodes[i].Name, heads[i].Hash, heads[i].Number, heads[i].StateRoot)
		}
		if heads[i].StateRoot != heads[0].StateRoot {
			// Should be unreachable while the hashes match, since the hash commits to
			// the root. If it ever fires, block hashing itself is wrong.
			return fmt.Errorf("same head %s but different state roots: %s=%s %s=%s",
				heads[0].Hash, d.nodes[0].Name, heads[0].StateRoot, d.nodes[i].Name, heads[i].StateRoot)
		}
	}
	if heads[0].Hash != want || uint64(heads[0].Number) != wantNum {
		return fmt.Errorf("head is %s at %d, expected %s at %d",
			heads[0].Hash, heads[0].Number, want, wantNum)
	}
	return d.assertNoBadBlocks(ctx)
}

// badBlock mirrors geth's BadBlockArgs. The RLP is the reason this oracle matters:
// it hands back a reproducible artifact rather than a log line.
type badBlock struct {
	Hash common.Hash     `json:"hash"`
	RLP  hexutil.Bytes   `json:"rlp"`
	Blk  json.RawMessage `json:"block"`
}

// assertNoBadBlocks is oracle 2. It reports only blocks rejected during *this* run:
// geth persists bad blocks to disk, so a datadir that has already seen a rejection
// (a previous run, or --self-test, which plants one on purpose) would otherwise fail
// every subsequent run for a divergence that is over.
func (d *Driver) assertNoBadBlocks(ctx context.Context) error {
	for i, n := range d.nodes {
		var bad []badBlock
		if err := n.RPC(ctx, "debug_getBadBlocks", &bad); err != nil {
			// Loud, not Debug: an oracle that has quietly stopped running is how a
			// harness ends up reporting a clean run it never actually checked.
			d.degrade(n.Name, "debug_getBadBlocks", err)
			continue
		}
		var fresh []badBlock
		for _, bb := range bad {
			if !d.knownBad[i][bb.Hash] {
				fresh = append(fresh, bb)
			}
		}
		if len(fresh) > 0 {
			for _, bb := range fresh {
				d.knownBad[i][bb.Hash] = true // report it once, not every slot
				d.saveArtifact(fmt.Sprintf("badblock-%s-%s.rlp", n.Name, bb.Hash.Hex()[:10]), bb.RLP)
				d.saveArtifact(fmt.Sprintf("badblock-%s-%s.json", n.Name, bb.Hash.Hex()[:10]), bb.Blk)
			}
			return fmt.Errorf("%s rejected %d new block(s) this run, first %s",
				n.Name, len(fresh), fresh[0].Hash)
		}
	}
	return nil
}

// baselineBadBlocks records what each node already considers bad, so only new
// rejections count as findings. Called at preflight and again after the self-test,
// which plants a rejected block of its own.
func (d *Driver) baselineBadBlocks(ctx context.Context) {
	d.knownBad = make([]map[common.Hash]bool, len(d.nodes))
	for i, n := range d.nodes {
		d.knownBad[i] = map[common.Hash]bool{}
		var bad []badBlock
		if err := n.RPC(ctx, "debug_getBadBlocks", &bad); err != nil {
			d.degrade(n.Name, "debug_getBadBlocks (baseline)", err)
			continue
		}
		for _, bb := range bad {
			d.knownBad[i][bb.Hash] = true
		}
		if len(bad) > 0 {
			slog.Info("baselined blocks this node already rejected; they will not be reported as findings",
				"node", n.Name, "count", len(bad))
		}
	}
}

// probe compares specific state between the nodes with eth_getProof — the only RPC
// that reads the binary tree per key (dumpBlock, accountRange and storageRangeAt are
// all refused on it).
//
// There is deliberately no debug_stateSize check: its tracker waits for snapshot
// generation to finish, and the binary tree has no generator and cannot have one, so
// it never initialises. A check that always skips itself is worse than no check.
func (d *Driver) probe(ctx context.Context) error {
	var problems []string

	// Prove the accounts the load actually created, not a fixed list. Anything the
	// hammer wrote lives at a random or CREATE-derived address, so a hardcoded set
	// would prove state nothing ever touched.
	for _, target := range d.proofTargets(ctx) {
		proofs := make([]json.RawMessage, len(d.nodes))
		ok := true
		for i, n := range d.nodes {
			if err := n.RPC(ctx, "eth_getProof", &proofs[i], target.addr, target.slots, "latest"); err != nil {
				d.degrade(n.Name, "eth_getProof", err)
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for i := 1; i < len(d.nodes); i++ {
			if jsonEqual(proofs[0], proofs[i]) {
				continue
			}
			problems = append(problems, fmt.Sprintf("eth_getProof for %s differs between %s and %s",
				target.addr, d.nodes[0].Name, d.nodes[i].Name))
			d.saveArtifact(fmt.Sprintf("proof-%s-%s.json", d.nodes[0].Name, target.addr[:10]), proofs[0])
			d.saveArtifact(fmt.Sprintf("proof-%s-%s.json", d.nodes[i].Name, target.addr[:10]), proofs[i])
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, " | "))
	}
	return nil
}

type proofTarget struct {
	addr  string
	slots []string
}

// proofTargets samples addresses out of the most recent block, so the proofs cover
// state the current load created, plus the system contracts the tree special-cases.
func (d *Driver) proofTargets(ctx context.Context) []proofTarget {
	targets := []proofTarget{
		{addr: "0x000F3df6D732807Ef1319fB7B8bB8522d0Beac02", slots: []string{zeroSlot}}, // beacon roots
		{addr: "0x0000F90827F1C53a10cb7A02335B175320002935", slots: []string{zeroSlot}}, // history storage
	}

	var blk struct {
		Transactions []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"transactions"`
	}
	if err := d.nodes[0].RPC(ctx, "eth_getBlockByNumber", &blk, "latest", true); err != nil {
		slog.Warn("cannot sample proof targets from the head block", "err", err)
		return targets
	}
	seen := map[string]bool{}
	for _, tx := range blk.Transactions {
		for _, a := range []string{tx.From, tx.To} {
			if a == "" || seen[a] || len(a) != 42 {
				continue
			}
			seen[a] = true
			targets = append(targets, proofTarget{addr: a, slots: []string{zeroSlot}})
		}
		if len(targets) >= 12 {
			break
		}
	}
	// Contracts created in this block are the freshest tree writes there are, and
	// their addresses appear only in receipts. One call covers the whole block.
	var receipts []struct {
		ContractAddress string `json:"contractAddress"`
	}
	if err := d.nodes[0].RPC(ctx, "eth_getBlockReceipts", &receipts, "latest"); err != nil {
		slog.Warn("cannot read block receipts for proof targets", "err", err)
		return targets
	}
	for _, r := range receipts {
		if r.ContractAddress == "" || seen[r.ContractAddress] {
			continue
		}
		seen[r.ContractAddress] = true
		targets = append(targets, proofTarget{addr: r.ContractAddress, slots: []string{zeroSlot}})
		if len(targets) >= 20 {
			break
		}
	}
	return targets
}

// awaitFirstBlock waits for the consensus clients to produce block 1. The self-test
// needs a real parent to build on; genesis has no state history behind it.
func (d *Driver) awaitFirstBlock(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Minute)
	logged := false
	for {
		blk, err := getBlock(ctx, d.nodes[0], "latest")
		if err == nil && blk.Number > 0 {
			d.head = blk.Hash
			d.headNum = uint64(blk.Number)
			d.headTime = uint64(blk.Timestamp)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no block was produced within 5m; are the consensus clients running?")
		}
		if !logged {
			slog.Info("waiting for the consensus clients to produce a first block")
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

const zeroSlot = "0x0000000000000000000000000000000000000000000000000000000000000000"

// selfTest proves the harness can actually detect a divergence, by manufacturing
// one. It builds a real payload, corrupts a single byte of its state root, and
// requires the node to reject it — then checks the evidence-capture path runs.
//
// Without this, "0 findings" is indistinguishable from a broken oracle, and every
// clean run is unfalsifiable. It runs against a healthy chain and leaves it healthy:
// the corrupted payload is never made canonical.
func (d *Driver) selfTest(ctx context.Context) error {
	slog.Info("self-test: manufacturing a divergence to prove the oracle detects it")

	// Wait for the consensus clients to produce a first block. Corrupting genesis
	// would prove nothing: the payload needs a real parent whose state the importers
	// already hold.
	if err := d.awaitFirstBlock(ctx); err != nil {
		return err
	}

	proposer := d.nodes[0]
	env, err := d.buildPayload(ctx, 1, proposer, d.head, d.headTime+12, d.cfg.feeRecipient)
	if err != nil {
		return fmt.Errorf("self-test could not build a payload: %w", err)
	}
	good := env.ExecutionPayload

	// Corrupt the state root, then recompute a block hash that matches the corrupted
	// header. Without that second step geth rejects on the hash check and never
	// executes, so the test would only prove that block hashing works. What we need
	// to exercise is the path that re-executes the block and compares the root it
	// computes against the one the payload commits to.
	// Randomise the corruption so a repeat run produces a different bad block. With a
	// fixed flip, the second run hits the node's bad-block cache and is rejected with
	// "links to previously rejected block" — a detection, but not one that re-runs the
	// root comparison we are trying to prove.
	beaconRoot := common.Hash{}
	bad := *good
	var flip [2]byte
	if _, err := rand.Read(flip[:]); err != nil {
		return fmt.Errorf("self-test: %w", err)
	}
	bad.StateRoot[int(flip[0])%len(bad.StateRoot)] ^= flip[1] | 0x01
	blk, err := engine.ExecutableDataToBlockNoHash(bad, nil, &beaconRoot, [][]byte{})
	if err != nil {
		return fmt.Errorf("self-test could not rebuild the corrupted block: %w", err)
	}
	bad.BlockHash = blk.Hash()

	// EVERY node other than the builder must refuse it. One node accepting a corrupted
	// root is enough to make the whole harness meaningless.
	for _, other := range d.nodes[1:] {
		var status engine.PayloadStatusV1
		err = other.Engine(ctx, "engine_newPayloadV5", &status,
			&bad, []common.Hash{}, &beaconRoot, []hexutil.Bytes{})
		switch {
		case err != nil:
			// An RPC-level rejection is also a detection, as long as it is not an outage.
			if IsTransportError(err) {
				return fmt.Errorf("self-test inconclusive: %s was unreachable: %w", other.Name, err)
			}
			slog.Info("self-test: corrupted payload rejected at the RPC layer", "node", other.Name, "err", err)
		case status.Status == engine.VALID:
			return fmt.Errorf("SELF-TEST FAILED: %s accepted a payload whose state root was corrupted — "+
				"the primary oracle does not work and no result from this harness means anything", other.Name)
		default:
			slog.Info("self-test: corrupted payload correctly refused",
				"node", other.Name, "status", status.Status, "err", deref(status.ValidationError))
		}
	}

	// Now prove the evidence path runs at all. It has otherwise never executed,
	// because it only fires on a finding.
	if err := d.captureDivergence(ctx, d.head); err != nil {
		return fmt.Errorf("self-test: evidence capture failed: %w", err)
	}
	slog.Info("self-test passed: divergence detected and evidence captured")
	return nil
}

// captureDivergence grabs everything that helps debug a finding, before the nodes are
// torn down: bad blocks with RLP, and every client's head.
func (d *Driver) captureDivergence(ctx context.Context, disputed common.Hash) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if disputed == (common.Hash{}) {
		disputed = d.head // nothing was in flight; fall back to the head
	}

	for _, n := range d.nodes {
		var bad []badBlock
		if err := n.RPC(ctx, "debug_getBadBlocks", &bad); err == nil {
			for _, bb := range bad {
				d.saveArtifact(fmt.Sprintf("badblock-%s-%s.rlp", n.Name, bb.Hash.Hex()[:10]), bb.RLP)
				d.saveArtifact(fmt.Sprintf("badblock-%s-%s.json", n.Name, bb.Hash.Hex()[:10]), bb.Blk)
			}
		}
		if blk, err := getBlock(ctx, n, "latest"); err == nil {
			blob, _ := json.MarshalIndent(blk, "", "  ")
			d.saveArtifact(fmt.Sprintf("head-%s.json", n.Name), blob)
		}
	}

	return nil
}

// artifactDir is where captured evidence lands: /artifacts in the container, where it
// is mounted out so it survives teardown. Overridable because the same binary is run
// directly on the host, where / is not writable.
var artifactDir = envOr("PBT_ARTIFACT_DIR", "/artifacts")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (d *Driver) saveArtifact(name string, blob []byte) {
	if len(blob) == 0 {
		return
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		// Losing the evidence for a real finding is serious enough to be loud.
		slog.Error("CANNOT SAVE EVIDENCE: artifact dir unwritable", "dir", artifactDir, "err", err)
		return
	}
	path := filepath.Join(artifactDir, name)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		slog.Warn("cannot write artifact", "path", path, "err", err)
		return
	}
	slog.Info("saved artifact", "path", path, "bytes", len(blob))
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	an, _ := json.Marshal(av)
	bn, _ := json.Marshal(bv)
	return string(an) == string(bn)
}
