// Command pbtdriver stands in for a consensus client and, in doing so, acts as a
// differential test harness for N execution clients running the EIP-8297 binary tree.
//
// One node builds a payload and EVERY node is asked to import it.
// engine_newPayloadV5 re-executes the block and compares the importer's own computed
// state root against the root the payload commits to, so a VALID from a node that did
// not build the block IS the state-root agreement assertion — delivered every block,
// with no polling and no root-scraping. Rotating the proposer means every node's
// block-building path is checked against every other node's validation path, which is
// where a same-binary devnet can actually diverge.
//
// Clients are given as repeated --el name=engineURL,rpcURL; at least two are required,
// since one node has nobody to disagree with.
//
// Engine versions are Amsterdam's: forkchoiceUpdatedV4 / getPayloadV6 /
// newPayloadV5. getPayloadV5 is gated to Osaka+BPO and will refuse an Amsterdam
// payload, so V6 is not optional here.
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

type config struct {
	slotTime     time.Duration
	slots        int
	feeRecipient common.Address
	reorgEvery   int
	reorgDepth   int
	probeEvery   int
	// verifyOracle runs the self-test before the slot loop. On by default: "0 findings"
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

		slotTime   = flag.Duration("slot-time", 3*time.Second, "time between blocks")
		slots      = flag.Int("slots", 0, "stop after N slots (0 = run forever)")
		reorgEvery = flag.Int("reorg-every", 0, "inject a competing-payload reorg every N slots (0 = never)")
		reorgDepth = flag.Int("reorg-depth", 1, "how many blocks the losing branch gets")
		probeEvery = flag.Int("probe-every", 16, "run the deeper cross-node probes every N slots")
		feeRecip   = flag.String("fee-recipient", "0x0000000000000000000000000000000000000001", "suggested fee recipient")
		verbose    = flag.Bool("v", false, "debug logging")
		verifyOrac = flag.Bool("verify-oracle", true, "prove the oracle detects a corrupted state root before starting the slot loop")
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
		slotTime:     *slotTime,
		slots:        *slots,
		feeRecipient: common.HexToAddress(*feeRecip),
		reorgEvery:   *reorgEvery,
		reorgDepth:   *reorgDepth,
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

// Driver owns the execution clients and the canonical head they are all driven to.
type Driver struct {
	nodes []*Node
	cfg   config

	head      common.Hash
	headNum   uint64
	headTime  uint64
	finalized common.Hash

	findings int
	// transient counts consecutive unreachable-node slots, so an outage that never
	// ends is still eventually reported rather than retried forever.
	transient int
	// disputed is the block the driver last tried to make canonical. On failure this
	// is the block to collect evidence about; d.head is still its parent, because
	// head only advances once both nodes have accepted.
	disputed common.Hash
	// knownBad is the per-node set of blocks already rejected before this run started,
	// so oracle 2 reports new rejections rather than history.
	knownBad []map[common.Hash]bool
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

	ticker := time.NewTicker(d.cfg.slotTime)
	defer ticker.Stop()

	for slot := 1; d.cfg.slots == 0 || slot <= d.cfg.slots; slot++ {
		select {
		case <-ctx.Done():
			slog.Info("stopping", "slots_done", slot-1, "findings", d.findings)
			return nil
		case <-ticker.C:
		}

		// Rotate the proposer so every node's builder output is validated by every
		// other node's importer. With N clients each block is checked N-1 times.
		proposer := d.nodes[slot%len(d.nodes)]
		importer := d.nodes[(slot+1)%len(d.nodes)]

		if err := d.advance(ctx, uint64(slot), proposer, importer); err != nil {
			if halt := d.handleErr(ctx, slot, "advance", err); halt != nil {
				return halt
			}
			continue
		}

		if d.cfg.reorgEvery > 0 && slot%d.cfg.reorgEvery == 0 {
			if err := d.injectReorg(ctx, uint64(slot), proposer, importer); err != nil {
				if halt := d.handleErr(ctx, slot, "reorg", err); halt != nil {
					return halt
				}
				continue
			}
		}

		if d.cfg.probeEvery > 0 && slot%d.cfg.probeEvery == 0 {
			if err := d.probe(ctx); err != nil {
				// Probes are advisory: they compare state the primary oracle has
				// already accepted, so a probe failure is recorded but does not stop
				// the run. Transport noise is not recorded at all.
				if IsTransportError(err) {
					slog.Warn("probe unreachable", "slot", slot, "err", err)
				} else {
					d.findings++
					slog.Error("FINDING: " + fmt.Sprintf("slot %d probe: %v", slot, err))
				}
			}
		}
	}
	slog.Info("done", "findings", d.findings)
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
		"head_number", d.headNum,
		"slot_time", d.cfg.slotTime)
	return nil
}

// advance builds one block on the proposer and requires both nodes to accept it.
func (d *Driver) advance(ctx context.Context, slot uint64, proposer, importer *Node) error {
	payload, err := d.buildPayload(ctx, slot, proposer, d.head, d.headTime+uint64(d.cfg.slotTime.Seconds()), d.cfg.feeRecipient)
	if err != nil {
		return err
	}
	data := payload.ExecutionPayload
	d.disputed = data.BlockHash

	// The oracle. Both nodes re-execute; the importer independently recomputes the
	// binary-tree root and compares it to data.StateRoot.
	statuses, err := d.newPayloadAll(ctx, data)
	if err != nil {
		return err
	}
	for i, st := range statuses {
		if st.Status != engine.VALID {
			return fmt.Errorf("%s rejected block %d (%s): status=%s latestValidHash=%v validationError=%s",
				d.nodes[i].Name, data.Number, data.BlockHash, st.Status, st.LatestValidHash, deref(st.ValidationError))
		}
	}

	if err := d.setHeadAll(ctx, data.BlockHash); err != nil {
		return err
	}

	// Heads must agree. Because the block hash commits to the state root, equal
	// hashes at equal height means equal roots.
	if err := d.assertHeadsAgree(ctx, data.BlockHash, data.Number); err != nil {
		return err
	}

	d.head = data.BlockHash
	d.headNum = data.Number
	d.headTime = data.Timestamp
	d.transient = 0 // both nodes answered and agreed; any past outage is over
	// Finality is ours to declare; keep it a few blocks back so reorgs stay legal.
	if data.Number > 8 {
		if blk, err := getBlock(ctx, proposer, hexutil.Uint64(data.Number-8).String()); err == nil {
			d.finalized = blk.Hash
		}
	}

	slog.Info("block",
		"slot", slot,
		"number", data.Number,
		"proposer", proposer.Name,
		"importer", importer.Name,
		"hash", data.BlockHash.TerminalString(),
		"state_root", data.StateRoot.TerminalString(),
		"txs", len(data.Transactions),
		"gas_used", data.GasUsed,
		"bal_bytes", len(data.BlockAccessList))
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

	// Give the builder most of a slot to actually pack transactions; asking
	// immediately yields an empty block and would fuzz nothing.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(d.cfg.slotTime * 2 / 3):
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

func (d *Driver) setHeadAll(ctx context.Context, head common.Hash) error {
	state := engine.ForkchoiceStateV1{
		HeadBlockHash:      head,
		SafeBlockHash:      d.finalized,
		FinalizedBlockHash: d.finalized,
	}
	for _, n := range d.nodes {
		var fcu engine.ForkChoiceResponse
		if err := n.Engine(ctx, "engine_forkchoiceUpdatedV4", &fcu, state, nil, nil); err != nil {
			return fmt.Errorf("set head on %s: %w", n.Name, err)
		}
		if fcu.PayloadStatus.Status != engine.VALID {
			return fmt.Errorf("set head on %s: status=%s err=%s",
				n.Name, fcu.PayloadStatus.Status, deref(fcu.PayloadStatus.ValidationError))
		}
	}
	return nil
}

// handleErr decides whether an error from the slot loop is a finding worth halting
// on, or just a node that went away. It returns non-nil only when the run should
// stop.
func (d *Driver) handleErr(ctx context.Context, slot int, phase string, err error) error {
	if IsTransportError(err) {
		// A node is restarting or otherwise unreachable. This is expected during
		// chaos testing and says nothing about the tree.
		d.transient++
		slog.Warn("node unreachable; waiting and resyncing",
			"slot", slot, "phase", phase, "consecutive", d.transient, "err", err)
		if d.transient > maxTransient {
			return fmt.Errorf("gave up after %d consecutive unreachable slots: %w", d.transient, err)
		}
		if rerr := d.resync(ctx); rerr != nil {
			if IsTransportError(rerr) {
				slog.Warn("resync not yet possible", "err", rerr)
				return nil
			}
			// resync found a real disagreement (a fork) while looking for the head.
			return d.report(ctx, slot, "resync", rerr)
		}
		return nil
	}

	return d.report(ctx, slot, phase, err)
}

// report records a divergence, captures the evidence for the disputed block, and
// returns the error that stops the run.
func (d *Driver) report(ctx context.Context, slot int, phase string, err error) error {
	d.transient = 0
	d.findings++
	slog.Error("FINDING: " + fmt.Sprintf("slot %d %s: %v", slot, phase, err))
	// d.head is still the parent at this point, since it only advances on success.
	// The disputed block is whatever the driver last tried to make canonical.
	if cerr := d.captureDivergence(ctx, d.disputed); cerr != nil {
		slog.Warn("capture failed", "err", cerr)
	}
	return fmt.Errorf("halting after divergence at slot %d (%s)", slot, phase)
}

// maxTransient bounds how long the driver waits out an unreachable node before it
// treats the outage itself as the failure.
const maxTransient = 40

// resync re-reads the head from every node and adopts it once they agree. A node
// that restarted may have come back at the last state its pbt.journal persisted
// rather than at the head the driver last set, so the driver's own view has to be
// refreshed instead of assumed.
func (d *Driver) resync(ctx context.Context) error {
	heads := make([]*rpcBlock, len(d.nodes))
	for i, n := range d.nodes {
		blk, err := getBlock(ctx, n, "latest")
		if err != nil {
			return err
		}
		heads[i] = blk
	}

	// A node at a different HEIGHT is behind, which an outage explains. A node at the
	// same height with a different HASH is a fork — the divergence this harness exists
	// to find — and must never be papered over by adopting one side.
	lower := 0
	for i := 1; i < len(d.nodes); i++ {
		if heads[i].Hash == heads[0].Hash {
			continue
		}
		if heads[i].Number == heads[0].Number {
			return fmt.Errorf("fork: %s and %s are both at height %d with different hashes: %s (root %s) vs %s (root %s)",
				d.nodes[0].Name, d.nodes[i].Name, heads[0].Number,
				heads[0].Hash, heads[0].StateRoot, heads[i].Hash, heads[i].StateRoot)
		}
		if heads[i].Number < heads[lower].Number {
			lower = i
		}
	}
	if heads[lower].Hash != heads[0].Hash || lower != 0 {
		slog.Warn("a node is behind after an outage; rebuilding from the lowest head",
			"node", d.nodes[lower].Name, "height", heads[lower].Number)
		d.head = heads[lower].Hash
		d.headNum = uint64(heads[lower].Number)
		d.headTime = uint64(heads[lower].Timestamp)
		return nil
	}
	d.head = heads[0].Hash
	d.headNum = uint64(heads[0].Number)
	d.headTime = uint64(heads[0].Timestamp)
	slog.Info("resynced", "head", d.head.TerminalString(), "number", d.headNum)
	return nil
}

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
