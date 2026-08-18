// Command pbtmonitor watches N execution clients running the EIP-8297 binary tree and
// reports any disagreement about state.
//
// Real consensus clients drive the chain; this process only observes it. Every tick it
// asks each client for the same block number and requires identical hashes, which on a
// chain whose block hash commits to the state root is a state-root assertion. Bad-block
// sets and eth_getProof samples are compared on a slower cadence.
//
// The one thing it does actively is prove its own oracle at startup: it builds a payload,
// corrupts a single byte of the state root, and requires every other client to reject it.
// Without that, "0 findings" from a broken oracle is indistinguishable from "0 findings"
// from a healthy chain. Neither half of that moves forkchoice or persists a block, so it
// is safe alongside real consensus clients.
//
// Clients are given as repeated --el name=engineURL,rpcURL; at least two are required,
// since one node has nobody to disagree with.
//
// Engine versions are Amsterdam's: forkchoiceUpdatedV4 / getPayloadV6 / newPayloadV5.
// getPayloadV5 is gated to Osaka+BPO and will refuse an Amsterdam payload, so V6 is not
// optional here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// emptyMPTRoot is keccak256(rlp("")) — the empty merkle-patricia root. Seeing it as
// a genesis state root would mean the node silently came up on the wrong commitment.
var emptyMPTRoot = common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// stalledTicks is how many consecutive polls may show the same head before saying so.
const stalledTicks = 10

// buildWait is how long the self-test lets a client pack transactions before asking
// for the payload.
const buildWait = 2 * time.Second

type config struct {
	pollInterval time.Duration
	feeRecipient common.Address
	probeEvery   int
	// verifyOracle runs the self-test before the follow loop. On by default: "0 findings"
	// from an oracle nobody proved is indistinguishable from "0 findings" from a broken
	// one, so every run earns the right to be believed before it starts.
	verifyOracle bool
	// expectedGenesisRoot, when set, is asserted against every node's genesis state
	// root. It is the client-agnostic way to prove the chain really is on the binary
	// tree; zero means "unchecked".
	expectedGenesisRoot common.Hash
}

// elFlag collects one `--el name=engineURL,rpcURL` entry per execution client.
type elFlag []struct{ name, engine, rpc string }

func (e *elFlag) String() string { return fmt.Sprint(*e) }

func (e *elFlag) Set(v string) error {
	name, urls, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want name=engineURL,rpcURL, got %q", v)
	}
	engine, rpc, ok := strings.Cut(urls, ",")
	if !ok {
		return fmt.Errorf("want name=engineURL,rpcURL, got %q", v)
	}
	*e = append(*e, struct{ name, engine, rpc string }{name, engine, rpc})
	return nil
}

func main() {
	var (
		els     elFlag
		jwtPath = flag.String("jwt", "/jwt/jwtsecret", "path to the engine API jwt secret")

		poll       = flag.Duration("poll", 2*time.Second, "how often to compare the clients' heads")
		probeEvery = flag.Int("probe-every", 16, "run the deeper cross-node probes every N polls")
		feeRecip   = flag.String("fee-recipient", "0x0000000000000000000000000000000000000001", "suggested fee recipient for the self-test payload")
		verbose    = flag.Bool("v", false, "debug logging")
		verifyOrac = flag.Bool("verify-oracle", true, "prove the oracle detects a corrupted state root before following the chain")
		selfTest   = flag.Bool("self-test", false, "run only that proof, then exit")
		expRoot    = flag.String("expected-genesis-root", "", "assert every node's genesis state root equals this (proves the binary-tree commitment)")
	)
	flag.Var(&els, "el", "execution client as name=engineURL,rpcURL (repeatable; at least two)")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Two is the minimum that means anything: with one node there is nobody to
	// disagree, and the whole design is that a block one node builds must satisfy
	// every other node.
	if len(els) < 2 {
		fatal("need at least two --el entries, got %d", len(els))
	}
	var nodes []*Node
	for _, el := range els {
		n, err := NewNode(el.name, el.engine, el.rpc, *jwtPath)
		if err != nil {
			fatal("%s: %v", el.name, err)
		}
		nodes = append(nodes, n)
	}

	cfg := config{
		pollInterval: *poll,
		feeRecipient: common.HexToAddress(*feeRecip),
		probeEvery:   *probeEvery,
		verifyOracle: *verifyOrac,
	}
	if *expRoot != "" {
		if len(*expRoot) != 66 || !strings.HasPrefix(*expRoot, "0x") {
			fatal("--expected-genesis-root must be a 0x-prefixed 32-byte hash, got %q", *expRoot)
		}
		cfg.expectedGenesisRoot = common.HexToHash(*expRoot)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := &Driver{nodes: nodes, cfg: cfg}

	if *selfTest {
		if err := d.preflight(ctx); err != nil {
			fatal("%v", err)
		}
		if err := d.proveOracle(ctx); err != nil {
			fatal("%v", err)
		}
		return
	}

	if err := d.Run(ctx); err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Driver owns the execution clients and the highest block they have all agreed on.
type Driver struct {
	nodes []*Node
	cfg   config

	head      common.Hash
	headNum   uint64
	headTime  uint64
	finalized common.Hash

	findings int
	// stalled counts consecutive polls that saw no new block, so a chain that stops
	// producing is reported once rather than every tick.
	stalled int
	// disputed is the block the clients last disagreed about, and the one evidence is
	// collected for.
	disputed common.Hash
	// knownBad is the per-node set of blocks already rejected before this run started,
	// so oracle 2 reports new rejections rather than history.
	knownBad []map[common.Hash]bool
	// degraded remembers which node/method pairs have already been reported as
	// unavailable. A client that does not implement an optional RPC does not
	// implement it on every tick either, and burying real findings under thousands of
	// identical warnings is its own kind of broken oracle.
	degraded map[string]bool
}

// degrade reports an unavailable oracle once per node and method.
func (d *Driver) degrade(node, method string, err error) {
	if d.degraded == nil {
		d.degraded = map[string]bool{}
	}
	key := node + "/" + method
	if d.degraded[key] {
		return
	}
	d.degraded[key] = true
	slog.Warn("ORACLE DEGRADED", "node", node, "method", method, "err", err,
		"note", "reported once; this oracle is skipped for the rest of the run")
}

func (d *Driver) Run(ctx context.Context) error {
	if err := d.preflight(ctx); err != nil {
		return err
	}
	if d.cfg.verifyOracle {
		if err := d.proveOracle(ctx); err != nil {
			return err
		}
	}
	return d.follow(ctx)
}

// follow watches the chain the consensus clients are driving. It never proposes and
// never sets a head: the only engine API calls this process makes after startup are
// the self-test's, and those neither move forkchoice nor persist a block.
//
// That restraint is the point. An earlier version drove the chain itself while real
// consensus clients drove it too, which manufactured competing blocks at every height,
// wedged one execution client, and then reported the resulting fork as a finding
// against the clients rather than against itself.
func (d *Driver) follow(ctx context.Context) error {
	ticker := time.NewTicker(d.cfg.pollInterval)
	defer ticker.Stop()

	slog.Info("following the chain", "nodes", len(d.nodes), "poll", d.cfg.pollInterval)

	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return d.summarise()
		case <-ticker.C:
		}
		ticks++

		if err := d.compareHeads(ctx); err != nil {
			if IsTransportError(err) {
				slog.Warn("node unreachable", "err", err)
			} else {
				d.finding("head comparison: %v", err)
			}
		}
		if d.cfg.probeEvery > 0 && ticks%d.cfg.probeEvery == 0 {
			if err := d.assertNoBadBlocks(ctx); err != nil {
				d.finding("%v", err)
			}
			if err := d.probe(ctx); err != nil {
				if IsTransportError(err) {
					slog.Warn("probe unreachable", "err", err)
				} else {
					d.finding("probe: %v", err)
				}
			}
		}
	}
}

// compareHeads asks every client for the same block number and requires identical
// hashes. The number is the lowest head across the clients, so a node that is merely
// one block behind is compared where it has actually reached rather than reported as
// a divergence.
func (d *Driver) compareHeads(ctx context.Context) error {
	heads := make([]*rpcBlock, len(d.nodes))
	lowest := ^uint64(0)
	for i, n := range d.nodes {
		blk, err := getBlock(ctx, n, "latest")
		if err != nil {
			return err
		}
		heads[i] = blk
		if uint64(blk.Number) < lowest {
			lowest = uint64(blk.Number)
		}
	}
	if lowest == 0 {
		return nil // nothing has been built yet
	}

	tag := hexutil.Uint64(lowest).String()
	at := make([]*rpcBlock, len(d.nodes))
	for i, n := range d.nodes {
		blk, err := getBlock(ctx, n, tag)
		if err != nil {
			return err
		}
		at[i] = blk
	}
	for i := 1; i < len(at); i++ {
		if at[i].Hash != at[0].Hash {
			d.disputed = at[0].Hash
			d.finding("clients disagree at block %d: %s=%s (root %s) %s=%s (root %s)",
				lowest,
				d.nodes[0].Name, at[0].Hash, at[0].StateRoot,
				d.nodes[i].Name, at[i].Hash, at[i].StateRoot)
			if err := d.captureDivergence(ctx, at[0].Hash); err != nil {
				slog.Warn("evidence capture failed", "err", err)
			}
			return nil
		}
	}

	if lowest > d.headNum {
		d.headNum = lowest
		d.head = at[0].Hash
		slog.Info("chain", "number", lowest, "hash", at[0].Hash.TerminalString(),
			"state_root", at[0].StateRoot.TerminalString(), "clients", len(d.nodes))
	} else if lowest == d.headNum {
		d.stalled++
		if d.stalled == stalledTicks {
			slog.Warn("chain has not advanced", "number", lowest, "ticks", d.stalled)
		}
		return nil
	}
	d.stalled = 0
	return nil
}

// finding records a divergence and keeps going. Halting on the first one ends a soak
// at the least convenient moment; the count is what the exit status reports.
func (d *Driver) finding(format string, args ...any) {
	d.findings++
	slog.Error("FINDING: " + fmt.Sprintf(format, args...))
}

func (d *Driver) summarise() error {
	slog.Info("stopping", "highest_block", d.headNum, "findings", d.findings)
	if d.findings > 0 {
		return fmt.Errorf("%d findings", d.findings)
	}
	return nil
}

// proveOracle runs the self-test on a chain that should be healthy, and leaves the
// harness able to report real findings afterwards.
func (d *Driver) proveOracle(ctx context.Context) error {
	before := d.findings
	if err := d.selfTest(ctx); err != nil {
		return err
	}
	if d.findings > before {
		return fmt.Errorf("self-test found %d real divergence(s) on a chain that should be healthy",
			d.findings-before)
	}
	// The self-test deliberately left a rejected block behind on every non-builder.
	// Oracle 2 reports blocks rejected *during* the run, so without re-baselining here
	// the first slot of the real loop reports our own planted block as a finding.
	d.baselineBadBlocks(ctx)
	return nil
}

// preflight checks every node is alive, on the same chain and genesis, on the same
// head, and — where the probe is conclusive — running the binary tree. Everything is
// compared against nodes[0].
func (d *Driver) preflight(ctx context.Context) error {
	boot, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	for _, n := range d.nodes {
		if err := n.WaitReady(boot); err != nil {
			return err
		}
	}

	chainIDs := make([]string, len(d.nodes))
	genesis := make([]*rpcBlock, len(d.nodes))
	for i, n := range d.nodes {
		if err := n.RPC(ctx, "eth_chainId", &chainIDs[i]); err != nil {
			return err
		}
		blk, err := getBlock(ctx, n, "0x0")
		if err != nil {
			return err
		}
		genesis[i] = blk
	}

	for i := 1; i < len(d.nodes); i++ {
		if chainIDs[i] != chainIDs[0] {
			return fmt.Errorf("chain id mismatch: %s=%s %s=%s",
				d.nodes[0].Name, chainIDs[0], d.nodes[i].Name, chainIDs[i])
		}
		if genesis[i].Hash != genesis[0].Hash {
			return fmt.Errorf("genesis mismatch: %s=%s %s=%s — the nodes are not on one chain",
				d.nodes[0].Name, genesis[0].Hash, d.nodes[i].Name, genesis[i].Hash)
		}
	}
	if genesis[0].StateRoot == emptyMPTRoot {
		return fmt.Errorf("genesis state root is the empty MPT root — genesis has no alloc, or pbt is off")
	}

	// Positive proof of the commitment, portable across clients: the genesis state
	// root must equal the expected binary-tree root.
	//
	// This replaces an earlier probe that called debug_dumpBlock and treated an error
	// as success, on the grounds that this geth branch refuses account dumping on the
	// tree. That inferred a property of the tree from one client's error string, and it
	// hard-failed any client that happens to serve the method — a barrier that had
	// nothing to do with whether the client implements PBT.
	if d.cfg.expectedGenesisRoot != (common.Hash{}) {
		if genesis[0].StateRoot != d.cfg.expectedGenesisRoot {
			return fmt.Errorf("genesis state root is %s, expected %s — the nodes are not committing state with the binary tree (is \"pbt\": true set?)",
				genesis[0].StateRoot, d.cfg.expectedGenesisRoot)
		}
		slog.Info("confirmed the binary-tree commitment", "genesis_state_root", genesis[0].StateRoot)
	} else {
		slog.Warn("no --expected-genesis-root given: the nodes agree with each other, but nothing checks they are on the binary tree at all")
	}

	// Start from wherever the nodes already are, not from genesis, so the driver can
	// be restarted against a running pair without trying to rebuild block 1.
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
			return fmt.Errorf("nodes start on different heads: %s=%s@%d %s=%s@%d",
				d.nodes[0].Name, heads[0].Hash, heads[0].Number,
				d.nodes[i].Name, heads[i].Hash, heads[i].Number)
		}
	}
	d.head = heads[0].Hash
	d.headNum = uint64(heads[0].Number)
	d.headTime = uint64(heads[0].Timestamp)
	d.finalized = common.Hash{}

	d.baselineBadBlocks(ctx)

	slog.Info("preflight ok",
		"chainid", chainIDs[0],
		"genesis", genesis[0].Hash,
		"genesis_state_root", genesis[0].StateRoot,
		"head", d.head,
		"head_number", d.headNum)
	return nil
}

// buildPayload runs the FCUv4-with-attributes then getPayloadV6 sequence.
func (d *Driver) buildPayload(ctx context.Context, slot uint64, n *Node, parent common.Hash, timestamp uint64, feeRecipient common.Address) (*engine.ExecutionPayloadEnvelope, error) {
	parentBlk, err := getBlockByHash(ctx, n, parent)
	if err != nil {
		return nil, err
	}
	// Amsterdam requires both of these in the attributes; omitting either makes the
	// forkchoice update fail rather than silently build something odd.
	slotNumber := slot
	targetGasLimit := uint64(parentBlk.GasLimit)
	beaconRoot := common.Hash{}

	attrs := &engine.PayloadAttributes{
		Timestamp:             timestamp,
		Random:                slotRandao(slot),
		SuggestedFeeRecipient: feeRecipient,
		Withdrawals:           []*types.Withdrawal{},
		BeaconRoot:            &beaconRoot,
		SlotNumber:            &slotNumber,
		TargetGasLimit:        &targetGasLimit,
	}
	state := engine.ForkchoiceStateV1{
		HeadBlockHash:      parent,
		SafeBlockHash:      d.finalized,
		FinalizedBlockHash: d.finalized,
	}

	var fcu engine.ForkChoiceResponse
	if err := n.Engine(ctx, "engine_forkchoiceUpdatedV4", &fcu, state, attrs, nil); err != nil {
		return nil, fmt.Errorf("forkchoiceUpdatedV4: %w", err)
	}
	if fcu.PayloadStatus.Status != engine.VALID {
		return nil, fmt.Errorf("forkchoiceUpdatedV4 on %s: status=%s err=%s",
			n.Name, fcu.PayloadStatus.Status, deref(fcu.PayloadStatus.ValidationError))
	}
	if fcu.PayloadID == nil {
		return nil, errors.New("forkchoiceUpdatedV4 returned no payload id")
	}

	// Give the builder a moment to pack transactions; asking immediately yields an
	// empty block. This runs once, in the self-test, so the wait is a fixed small
	// value rather than a fraction of a slot we no longer control.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(buildWait):
	}

	var envelope engine.ExecutionPayloadEnvelope
	// V6 is the Amsterdam getPayload. V5 is gated to Osaka/BPO and will refuse.
	if err := n.Engine(ctx, "engine_getPayloadV6", &envelope, fcu.PayloadID); err != nil {
		return nil, fmt.Errorf("getPayloadV6: %w", err)
	}
	if envelope.ExecutionPayload == nil {
		return nil, errors.New("getPayloadV6 returned no payload")
	}
	if envelope.ExecutionPayload.BlockAccessList == nil {
		// newPayloadV5 refuses a payload whose block access list is missing, so
		// catching it here gives a clearer message than the import failure would.
		return nil, errors.New("payload carries no block access list (EIP-7928)")
	}
	return &envelope, nil
}

// newPayloadAll imports one payload into every node concurrently. Each node
// re-executes it and compares its own computed state root against the one the
// payload commits to, which is what makes this the primary oracle.
func (d *Driver) newPayloadAll(ctx context.Context, data *engine.ExecutableData) ([]engine.PayloadStatusV1, error) {
	var (
		statuses = make([]engine.PayloadStatusV1, len(d.nodes))
		errs     = make([]error, len(d.nodes))
		wg       sync.WaitGroup
	)
	beaconRoot := common.Hash{}
	for i, n := range d.nodes {
		wg.Add(1)
		go func(i int, n *Node) {
			defer wg.Done()
			errs[i] = n.Engine(ctx, "engine_newPayloadV5", &statuses[i],
				data, []common.Hash{}, &beaconRoot, []hexutil.Bytes{})
		}(i, n)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			return statuses, fmt.Errorf("newPayloadV5 on %s: %w", d.nodes[i].Name, errs[i])
		}
	}
	return statuses, nil
}

// maxTransient bounds how long the driver waits out an unreachable node before it
// treats the outage itself as the failure.
const maxTransient = 40

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

// slotRandao gives each slot a distinct, reproducible prevRandao so blocks at the
// same height on competing branches differ.
func slotRandao(slot uint64) common.Hash {
	var h common.Hash
	h[0] = byte(slot)
	h[1] = byte(slot >> 8)
	h[31] = 0xa1
	return h
}
