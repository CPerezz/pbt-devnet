// Command hammer generates transactions aimed at what EIP-8297 changed, rather than at
// throughput. A generic spammer moves ether between existing accounts, which touches one
// leaf each and says little about a new state commitment.
//
//	fanout      fresh recipients                 -> new header stems
//	storage     spread storage slots             -> the storage zone and its 66-byte keys
//	codedup     identical code, several senders  -> code-zone leaves SHARED between accounts,
//	                                               the property that makes rollback impossible
//	destruct    deploy a child with code, then
//	            destroy it in the same tx        -> code-zone leaves nothing reclaims
//	delegate    7702 delegate, re-delegate,
//	            then clear to the zero address   -> the delegation leaf and its removal
//	zeroize     write slots, then store zero     -> deletion, because zero IS absence
//	callread    CALL a contract that SLOADs      -> the read path, present and absent leaves
//	extcode     EXTCODESIZE / EXTCODECOPY over
//	            a large contract                 -> code-zone reads of a blob spread
//	                                               across many chunk leaves
//	legacy      type 0 envelope                  -> the pre-2930 path
//	accesslist  type 1, populated list           -> repriced access-list accounting
//	revert      write slots, then REVERT         -> intra-transaction rollback of tree writes
//
// Every workload only generates traffic. The assertion is the driver's: each block one
// node builds must be accepted by all the others, so a shape that makes two nodes compute
// different roots shows up there. That means these workloads catch a divergence, not a
// uniformly wrong implementation — with one client, agreement is all there is to check.
//
// All bytecode is straight-line with no jumps, so it needs no compiler.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/hammer/txkit"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
)

// fallbackKeys are the standard local-development keys. They are only useful on a genesis
// that funds them; under Kurtosis the sender keys are passed in with --key, taken from
// whatever accounts the network actually prefunded.
var fallbackKeys = []string{
	"ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	"59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
	"5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a",
	"7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
	"47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a",
	"8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba",
}

// allWorkloads is the full mix, run round-robin in this order. There is deliberately no
// selection knob beyond --only: a default that covers everything is worth more than a
// tunable one nobody tunes.
var allWorkloads = []string{
	"fanout", "storage", "codedup", "destruct",
	"delegate", "zeroize", "callread", "extcode",
	"legacy", "accesslist", "revert",
}

type sender struct {
	key   *ecdsa.PrivateKey
	addr  common.Address
	nonce uint64
}

func main() {
	var (
		rpcURLs    multiFlag
		senderKeys multiFlag
		chainID    = flag.Int64("chainid", 0, "chain id (0 = ask the node)")
		rate       = flag.Duration("interval", 400*time.Millisecond, "time between batches")
		batch      = flag.Int("batch", 4, "transactions per batch")
		slots      = flag.Int("slots-per-tx", 24, "storage slots written per storage tx")
		codeSize   = flag.Int("code-size", 12_000, "runtime code size for the codedup and extcode targets")
		only       = flag.String("only", "", "run only this workload")
		duration   = flag.Duration("for", 0, "stop after this long (0 = run forever)")
		gasTipCap  = flag.Int64("tip", 1_000_000_000, "max priority fee per gas, wei")
		startWait  = flag.Duration("startup-timeout", 4*time.Minute, "how long to wait for the startup targets to be mined")
	)
	flag.Var(&rpcURLs, "rpc", "execution rpc endpoint (repeatable; every tx is sent to all of them)")
	flag.Var(&senderKeys, "key", "hex private key to send from (repeatable; defaults to the standard dev keys)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(rpcURLs) == 0 {
		rpcURLs = multiFlag{"http://127.0.0.1:8545"}
	}
	// EIP-170 caps deployed code at 24576 bytes. Past that every codedup transaction
	// fails gas estimation forever, which looks like a stuck generator rather than a
	// bad flag.
	if *codeSize < 1 || *codeSize > 24576 {
		fatal("--code-size %d is outside 1..24576 (EIP-170 max deployed code size)", *codeSize)
	}

	ctx := context.Background()
	var clients []*ethclient.Client
	for _, u := range rpcURLs {
		c, err := ethclient.DialContext(ctx, u)
		if err != nil {
			fatal("dial %s: %v", u, err)
		}
		clients = append(clients, c)
		slog.Info("connected", "rpc", u)
	}

	// The chain id comes from the node unless pinned. Under Kurtosis the genesis generator
	// picks it, so a hardcoded default would sign transactions no node accepts.
	chain := big.NewInt(*chainID)
	if *chainID == 0 {
		got, err := chainIDWhenReady(ctx, clients[0])
		if err != nil {
			fatal("could not read the chain id: %v", err)
		}
		chain = got
		slog.Info("chain id from node", "chainid", chain)
	}

	keys := []string(senderKeys)
	if len(keys) == 0 {
		keys = fallbackKeys
		slog.Info("no --key given, using the standard dev keys", "count", len(keys))
	}

	signer := types.LatestSignerForChainID(chain)
	senders := make([]*sender, 0, len(keys))
	for _, hexKey := range keys {
		key, err := crypto.HexToECDSA(hexKey)
		if err != nil {
			fatal("bad dev key: %v", err)
		}
		addr := crypto.PubkeyToAddress(key.PublicKey)
		// Wait for the node rather than exiting: the generator is normally started
		// alongside the nodes and will lose the race to bind otherwise.
		nonce, err := nonceWhenReady(ctx, clients[0], addr)
		if err != nil {
			fatal("nonce for %s: %v", addr, err)
		}
		senders = append(senders, &sender{key: key, addr: addr, nonce: nonce})
		slog.Info("sender ready", "addr", addr, "nonce", nonce)
	}

	// One fixed blob, reused across senders on purpose: identical code means an
	// identical code hash, which means the code-zone leaves are shared. That shared
	// ownership is the thing no per-account history can describe.
	sharedCode := txkit.PatternCode(*codeSize, 0x5f)

	head, err := clients[0].HeaderByNumber(ctx, nil)
	if err != nil {
		fatal("head: %v", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(1_000_000_000)
	}
	gasFeeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(4)), big.NewInt(*gasTipCap))

	workloads := allWorkloads
	if *only != "" {
		if !slices.Contains(allWorkloads, *only) {
			fatal("--only %q is not a workload; choose from %v", *only, allWorkloads)
		}
		workloads = []string{*only}
	}

	// Four of the workloads need contracts to aim at. Deploying them once here, and
	// waiting for them to be mined, is what lets those workloads use a plain address
	// instead of re-deploying a target per transaction.
	env := &world{
		signer:    signer,
		chainID:   chain,
		gasTipCap: big.NewInt(*gasTipCap),
		gasFeeCap: gasFeeCap,
		slots:     *slots,
		code:      sharedCode,
	}
	if err := env.deployTargets(ctx, clients, senders[0], *startWait); err != nil {
		fatal("startup targets: %v", err)
	}
	env.authorities = newAuthorities(16)

	deadline := time.Time{}
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
	}

	ticker := time.NewTicker(*rate)
	defer ticker.Stop()

	stats := map[string]int{}
	round := 0
	for range ticker.C {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		round++
		kind := workloads[round%len(workloads)]

		for i := 0; i < *batch; i++ {
			s := senders[(round*(*batch)+i)%len(senders)]

			tx, err := env.buildTx(ctx, clients, kind, s)
			if err != nil {
				slog.Warn("build failed", "workload", kind, "err", err)
				continue
			}
			signed, err := types.SignTx(tx, signer, s.key)
			if err != nil {
				slog.Warn("sign failed", "err", err)
				continue
			}

			if !broadcast(ctx, clients, signed, kind, s) {
				// Every node refused: resync from the chain rather than drifting further.
				if n, nerr := clients[0].PendingNonceAt(ctx, s.addr); nerr == nil {
					s.nonce = n
				}
				continue
			}
			s.nonce++
			stats[kind]++
		}

		if round%25 == 0 {
			slog.Info("sent", "total", total(stats), "by_workload", summary(stats))
			checkNonceDrift(ctx, clients, senders)
			env.gasFeeCap = refreshFeeCap(ctx, clients[0], env.gasFeeCap, *gasTipCap)
		}
	}
	slog.Info("hammer done", "total", total(stats), "by_workload", summary(stats))
}

// world is everything the workloads need beyond the sender: fee levels, the contracts
// deployed at startup, and the 7702 authority pool.
type world struct {
	signer    types.Signer
	chainID   *big.Int
	gasTipCap *big.Int
	gasFeeCap *big.Int
	slots     int
	code      []byte

	// reader SLOADs a slot named by calldata; writer SSTOREs a calldata key/value pair;
	// bigcode is a large inert blob. All three are deployed once at startup.
	reader  common.Address
	writer  common.Address
	bigcode common.Address

	authorities []*authority

	// liveSlots are keys currently non-zero in the writer contract, so the cross-
	// transaction half of the zeroize workload has something real to delete.
	liveSlots []common.Hash
	// zeroizeTurn alternates that workload between its same-transaction and
	// cross-transaction shapes; readTurn walks callread across present and absent keys.
	// Separate counters on purpose: sharing one would make each workload's coverage
	// depend on how often the other ran.
	zeroizeTurn int
	readTurn    int
}

// authority is a 7702 delegation target account. Authorities are separate from the
// transaction senders on purpose: an authorization carries the AUTHORITY's nonce, and
// coupling that to a sender's nonce makes every failed send corrupt the next
// authorization.
type authority struct {
	key  *ecdsa.PrivateKey
	addr common.Address
	// phase cycles delegate -> re-delegate -> clear.
	phase int
	// lastNonce is the authorization nonce used last time, and used is whether there was
	// a last time. Together they detect an authorization that never landed: 7702 skips
	// an invalid authorization silently, leaving a status=1 transaction that did nothing.
	lastNonce uint64
	used      bool
}

func newAuthorities(n int) []*authority {
	out := make([]*authority, 0, n)
	for i := 0; i < n; i++ {
		key, err := crypto.GenerateKey()
		if err != nil {
			fatal("generating an authority key: %v", err)
		}
		out = append(out, &authority{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)})
	}
	slog.Info("authority pool ready", "count", n)
	return out
}

// deployTargets deploys the three fixed contracts and waits for them to be mined.
//
// It blocks because the workloads that use them cannot start without an address, and a
// silent fallback would be worse: aiming EXTCODESIZE at an empty account still produces
// valid blocks that agree, so the workload would look healthy while testing nothing.
func (w *world) deployTargets(ctx context.Context, clients []*ethclient.Client, s *sender, wait time.Duration) error {
	specs := []struct {
		name string
		dst  *common.Address
		init []byte
	}{
		// The reader pre-writes a handful of slots in its own initcode, so callread has
		// both present and absent leaves to read rather than only absent ones.
		{"reader", &w.reader, txkit.DeployCodeAfter(txkit.SeedCode(txkit.ReaderSeeded), txkit.ReaderRuntime())},
		{"writer", &w.writer, txkit.DeployCodeAfter(nil, txkit.WriterRuntime())},
		{"bigcode", &w.bigcode, txkit.DeployCodeAfter(nil, w.code)},
	}

	hashes := make([]common.Hash, len(specs))
	for i, sp := range specs {
		gas, err := estimateEverywhere(ctx, clients, ethereum.CallMsg{
			From: s.addr, Data: sp.init, Value: big.NewInt(0),
			GasFeeCap: w.gasFeeCap, GasTipCap: w.gasTipCap,
		})
		if err != nil {
			return fmt.Errorf("estimating %s: %w", sp.name, err)
		}
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID: w.chainID, Nonce: s.nonce, GasTipCap: w.gasTipCap, GasFeeCap: w.gasFeeCap,
			Gas: gas + gas/100, Data: sp.init, Value: big.NewInt(0),
		})
		signed, err := types.SignTx(tx, w.signer, s.key)
		if err != nil {
			return fmt.Errorf("signing %s: %w", sp.name, err)
		}
		if !broadcast(ctx, clients, signed, "deploy:"+sp.name, s) {
			return fmt.Errorf("no node accepted the %s deployment", sp.name)
		}
		hashes[i] = signed.Hash()
		s.nonce++
	}

	deadline := time.Now().Add(wait)
	for i, sp := range specs {
		rcpt, err := waitReceipt(ctx, clients[0], hashes[i], deadline)
		if err != nil {
			return fmt.Errorf("%s (%s): %w — is the driver producing blocks?", sp.name, hashes[i], err)
		}
		if rcpt.Status != types.ReceiptStatusSuccessful {
			return fmt.Errorf("%s deployment reverted (tx %s, gas used %d)", sp.name, hashes[i], rcpt.GasUsed)
		}
		*sp.dst = rcpt.ContractAddress
		slog.Info("startup target deployed", "name", sp.name, "addr", rcpt.ContractAddress,
			"block", rcpt.BlockNumber, "gas_used", rcpt.GasUsed)
	}
	return nil
}

// waitReceipt polls until the transaction is mined. It reports progress so a devnet that
// is not producing blocks looks stuck rather than silent.
//
// Every error is retried, not just ethereum.NotFound. A node that has just started also
// answers "transaction indexing is in progress", and treating anything other than
// NotFound as fatal turned that into a hammer that refused to start.
func waitReceipt(ctx context.Context, cl *ethclient.Client, hash common.Hash, deadline time.Time) (*types.Receipt, error) {
	var last error
	lastLog := time.Time{}
	for {
		rcpt, err := cl.TransactionReceipt(ctx, hash)
		if err == nil {
			return rcpt, nil
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("not mined within the startup timeout (last: %w)", last)
		}
		if time.Since(lastLog) > 15*time.Second {
			why := "not mined yet"
			if !errors.Is(err, ethereum.NotFound) {
				why = err.Error()
			}
			slog.Info("waiting for a startup target", "tx", hash, "status", why)
			lastLog = time.Now()
		}
		time.Sleep(time.Second)
	}
}

// broadcast sends to EVERY node, not one of them. The nodes are unpeered, so a
// transaction submitted to only one leaves the other with a nonce gap; the gapped
// transaction sits in the queued subpool, is capped at 64 per account, and the surplus is
// dropped — with no error returned. Two thirds of the load used to disappear this way.
// Broadcasting is what devp2p gossip would have done for us.
func broadcast(ctx context.Context, clients []*ethclient.Client, tx *types.Transaction, kind string, s *sender) bool {
	accepted := false
	for _, cl := range clients {
		if err := cl.SendTransaction(ctx, tx); err != nil {
			slog.Warn("send failed", "workload", kind, "from", s.addr, "nonce", tx.Nonce(), "err", err)
			continue
		}
		accepted = true
	}
	return accepted
}

// chainIDWhenReady polls until the node answers, for the same reason as nonceWhenReady:
// the generator is started alongside the nodes it drives and will lose the race otherwise.
func chainIDWhenReady(ctx context.Context, cl *ethclient.Client) (*big.Int, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		id, err := cl.ChainID(ctx)
		if err == nil {
			return id, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(time.Second)
	}
}

// nonceWhenReady polls until the node answers, so the generator can be started at
// the same moment as the nodes it drives.
func nonceWhenReady(ctx context.Context, cl *ethclient.Client, addr common.Address) (uint64, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		nonce, err := cl.PendingNonceAt(ctx, addr)
		if err == nil {
			return nonce, nil
		}
		if time.Now().After(deadline) {
			return 0, err
		}
		time.Sleep(time.Second)
	}
}

// maxNonceDrift is how far the local counter may run ahead of the chain before we
// treat it as transactions being dropped rather than merely queued.
const maxNonceDrift = 128

// checkNonceDrift compares what we think we have sent against what each node will
// actually execute next.
//
// This exists because geth accepts a transaction it cannot execute yet, returns no
// error, and later evicts it silently once the per-account queue is full. Without
// this check the generator reports thousands of sends while the chain executes a
// fraction of them, and nothing anywhere says so.
func checkNonceDrift(ctx context.Context, clients []*ethclient.Client, senders []*sender) {
	for _, s := range senders {
		for _, cl := range clients {
			pending, err := cl.PendingNonceAt(ctx, s.addr)
			if err != nil {
				continue
			}
			if s.nonce > pending+maxNonceDrift {
				slog.Error("NONCE DRIFT: transactions are being dropped, not queued",
					"sender", s.addr, "local", s.nonce, "node_pending", pending,
					"drift", s.nonce-pending)
			}
		}
	}
}

// refreshFeeCap re-reads the base fee so a long run cannot be priced out. The cap is
// only ever raised: lowering it mid-run would make already-queued transactions
// unreplaceable.
func refreshFeeCap(ctx context.Context, cl *ethclient.Client, current *big.Int, tip int64) *big.Int {
	head, err := cl.HeaderByNumber(ctx, nil)
	if err != nil || head.BaseFee == nil {
		return current
	}
	want := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(4)), big.NewInt(tip))
	if want.Cmp(current) > 0 {
		slog.Info("raising fee cap", "from", current, "to", want)
		return want
	}
	return current
}

// estimateEverywhere asks every client to price the same call and returns the largest
// answer. A disagreement is logged as a finding: on a chain where gas has a state
// dimension, two implementations pricing one call differently is a consensus-relevant
// difference, not a rounding detail.
func estimateEverywhere(ctx context.Context, clients []*ethclient.Client, call ethereum.CallMsg) (uint64, error) {
	var (
		best  uint64
		first uint64
		have  bool
		errs  []string
	)
	for i, cl := range clients {
		g, err := cl.EstimateGas(ctx, call)
		if err != nil {
			errs = append(errs, fmt.Sprintf("client %d: %v", i, err))
			continue
		}
		if !have {
			first, have = g, true
		} else if g != first {
			slog.Error("FINDING: clients disagree on gas for the same call",
				"client_0", first, "client_"+fmt.Sprint(i), g, "to", call.To, "data_len", len(call.Data))
		}
		if g > best {
			best = g
		}
	}
	if !have {
		return 0, fmt.Errorf("no client could estimate gas: %s", strings.Join(errs, "; "))
	}
	return best, nil
}

// envelope selects the transaction type. It is deliberately NOT types.LegacyTxType and
// friends: those start at 0, so a shape that forgot to set one would silently become a
// legacy transaction. It happened — nine of the eleven workloads went out as type 0 and
// the 1559 path went untested, with nothing failing anywhere. Here the zero value is the
// one almost everything wants.
type envelope int

const (
	env1559 envelope = iota
	envLegacy
	envAccessList
	envSetCode
)

// shape is what a workload decides: where the transaction goes, what it carries, and
// which envelope holds it. Pricing and signing are common to all of them.
type shape struct {
	to    *common.Address
	data  []byte
	value *big.Int
	// priceData, when set, is what gets estimated instead of data. Only the revert
	// workload needs it — see buildTx.
	priceData  []byte
	accessList types.AccessList
	auth       []types.SetCodeAuthorization
	envelope   envelope
}

func (w *world) buildTx(ctx context.Context, clients []*ethclient.Client, kind string, s *sender) (*types.Transaction, error) {
	sh, err := w.shapeFor(ctx, clients, kind, s)
	if err != nil {
		return nil, err
	}

	// Gas is asked for, not guessed: on this chain a bare value transfer to a fresh
	// account costs far more than the classic 21k (measured, see README), so a
	// hardcoded limit sends every transaction out-of-gas — where it still lands in a
	// block, still burns the whole limit, and looks like load while testing nothing.
	//
	// Every client is asked, not just the first. With heterogeneous clients an estimate
	// from one is not valid for another, and on a two-dimensional gas model a
	// disagreement about the cost of the same call is itself a divergence worth
	// reporting. The largest estimate is used so the transaction is executable
	// everywhere.
	priced := sh.data
	if sh.priceData != nil {
		priced = sh.priceData
	}
	gas, err := estimateEverywhere(ctx, clients, ethereum.CallMsg{
		From:              s.addr,
		To:                sh.to,
		Value:             sh.value,
		Data:              priced,
		GasFeeCap:         w.gasFeeCap,
		GasTipCap:         w.gasTipCap,
		AccessList:        sh.accessList,
		AuthorizationList: sh.auth,
	})
	if err != nil {
		return nil, err
	}
	// 1% only. A wide margin would paper over exactly what we want to see: if the
	// estimate does not match execution, that gap is a finding about the gas model,
	// not something to absorb silently.
	gas = gas + gas/100

	switch sh.envelope {
	case envLegacy:
		return types.NewTx(&types.LegacyTx{
			Nonce:    s.nonce,
			GasPrice: w.gasFeeCap, // a legacy tx has one price, which must cover the base fee
			Gas:      gas,
			To:       sh.to,
			Value:    sh.value,
			Data:     sh.data,
		}), nil

	case envAccessList:
		return types.NewTx(&types.AccessListTx{
			ChainID:    w.chainID,
			Nonce:      s.nonce,
			GasPrice:   w.gasFeeCap,
			Gas:        gas,
			To:         sh.to,
			Value:      sh.value,
			Data:       sh.data,
			AccessList: sh.accessList,
		}), nil

	case envSetCode:
		// SetCodeTx.To is not a pointer: a type-4 transaction cannot be a creation.
		if sh.to == nil {
			return nil, fmt.Errorf("a setcode transaction needs a destination")
		}
		return types.NewTx(&types.SetCodeTx{
			ChainID:   uint256.MustFromBig(w.chainID),
			Nonce:     s.nonce,
			GasTipCap: uint256.MustFromBig(w.gasTipCap),
			GasFeeCap: uint256.MustFromBig(w.gasFeeCap),
			Gas:       gas,
			To:        *sh.to,
			Value:     uint256.MustFromBig(sh.value),
			Data:      sh.data,
			AuthList:  sh.auth,
		}), nil

	default:
		return types.NewTx(&types.DynamicFeeTx{
			ChainID:    w.chainID,
			Nonce:      s.nonce,
			GasTipCap:  w.gasTipCap,
			GasFeeCap:  w.gasFeeCap,
			Gas:        gas,
			To:         sh.to,
			Value:      sh.value,
			Data:       sh.data,
			AccessList: sh.accessList,
		}), nil
	}
}

func (w *world) shapeFor(ctx context.Context, clients []*ethclient.Client, kind string, s *sender) (*shape, error) {
	sh := &shape{value: big.NewInt(0)}

	switch kind {
	case "fanout":
		// A brand-new random recipient every time, so every one of these adds a
		// header stem the tree has never held.
		addr, err := txkit.RandomAddress()
		if err != nil {
			return nil, err
		}
		sh.to, sh.value = &addr, big.NewInt(1)

	case "storage":
		// Creation whose initcode writes `slots` scattered storage slots and returns
		// empty code: a fresh account plus a wide scatter of storage-zone leaves.
		sh.data = txkit.StorageInitCode(w.slots)

	case "codedup":
		// Same runtime code from a rotating set of senders: identical code hash,
		// shared code-zone leaves.
		sh.data = txkit.DeployCodeAfter(nil, w.code)

	case "destruct":
		// A modest child so the workload stays cheap next to codedup; the point is
		// that code exists at all when the account disappears.
		sh.data = txkit.FactoryDestructInitCode(s.addr, 2048)

	case "delegate":
		return w.delegateShape(ctx, clients)

	case "zeroize":
		return w.zeroizeShape(), nil

	case "callread":
		// CALL the reader, which SLOADs the slot named by calldata. Half the keys are
		// ones it seeded in its own initcode (a present leaf), half are not (an absent
		// one) — two different walks through the tree.
		key := txkit.ReaderSeeded[w.readTurn%len(txkit.ReaderSeeded)]
		if w.readTurn%2 == 1 {
			key += 1 << 32 // far outside anything the reader ever wrote
		}
		w.readTurn++
		to := w.reader
		sh.to, sh.data = &to, txkit.SlotKey(key).Bytes()

	case "extcode":
		// Reading only the SIZE of a large contract still makes the client resolve the
		// whole blob, which on this tree means touching every code-zone chunk leaf the
		// contract occupies. Cheap in gas, wide in state access — that is the shape.
		sh.data = txkit.ExtcodeInitCode(w.bigcode, len(w.code), 8)

	case "legacy":
		// Type 0, EIP-155 protected. A plain transfer keeps the envelope the variable.
		addr, err := txkit.RandomAddress()
		if err != nil {
			return nil, err
		}
		sh.to, sh.value, sh.envelope = &addr, big.NewInt(1), envLegacy

	case "accesslist":
		// Type 1 with a POPULATED list, which is the point: Amsterdam reprices the
		// per-address and per-storage-key charges, so an empty list would exercise
		// nothing the 1559 path does not.
		to := w.reader
		sh.to = &to
		sh.data = txkit.SlotKey(txkit.ReaderSeeded[0]).Bytes()
		sh.envelope = envAccessList
		sh.accessList = types.AccessList{{
			Address: w.reader,
			StorageKeys: []common.Hash{
				txkit.SlotKey(txkit.ReaderSeeded[0]), txkit.SlotKey(txkit.ReaderSeeded[1]), txkit.SlotKey(1 << 40),
			},
		}}

	case "revert":
		// Writes a batch of slots and then reverts, so the tree must unwind them.
		//
		// eth_estimateGas cannot price this: it searches for a gas value that makes the
		// call SUCCEED, and a transaction that always reverts has none, so estimation
		// fails and the workload would loop on "build failed". The non-reverting twin
		// does identical work and ends in RETURN instead of REVERT, so its estimate is
		// the right number — which keeps the rule that gas is never hardcoded.
		writes := txkit.ZeroizeSlots()
		sh.data = append(txkit.SeedCode(writes), 0x60, 0x00, 0x60, 0x00, 0xfd)      // ... REVERT
		sh.priceData = append(txkit.SeedCode(writes), 0x60, 0x00, 0x60, 0x00, 0xf3) // ... RETURN

	default:
		return nil, fmt.Errorf("unknown workload %q", kind)
	}
	return sh, nil
}

// delegateShape produces one step of the 7702 lifecycle for the next authority in the
// pool: delegate, re-delegate elsewhere, then clear back to the zero address.
//
// The clear is the reason this workload exists. It is the one path that has to delete the
// delegation leaf and restore the empty code hash, and no native test drives it with the
// binary tree enabled.
func (w *world) delegateShape(ctx context.Context, clients []*ethclient.Client) (*shape, error) {
	a := w.authorities[0]
	w.authorities = append(w.authorities[1:], a) // round-robin, so each authority rests

	// The authorization carries the authority's own nonce, read from the chain. A stale
	// value is not an error anywhere: 7702 skips an invalid authorization and the
	// transaction still succeeds, so the delegation would simply never happen.
	nonce, err := clients[0].PendingNonceAt(ctx, a.addr)
	if err != nil {
		return nil, fmt.Errorf("nonce for authority %s: %w", a.addr, err)
	}
	if a.used && nonce == a.lastNonce {
		// The previous authorization for this account never landed. Ours to fix, not the
		// client's: the round-robin came back to this authority before its last
		// authorization was mined. Expected under `--only delegate`, where nothing else
		// shares the traffic; a warning in the default mix means blocks have stalled.
		slog.Warn("authorization did not land; the pool is rotating faster than blocks",
			"authority", a.addr, "nonce", nonce)
	}

	var target common.Address
	switch a.phase % 3 {
	case 0:
		target = w.bigcode // delegate to something with real code
	case 1:
		target = w.reader // re-delegate: overwrite an existing delegation leaf
	case 2:
		target = common.Address{} // clear: delete the delegation leaf
	}
	a.phase++
	a.lastNonce, a.used = nonce, true

	auth, err := types.SignSetCode(a.key, types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(w.chainID),
		Address: target,
		Nonce:   nonce,
	})
	if err != nil {
		return nil, err
	}

	// Send the transaction TO the authority, so the delegated code is actually executed
	// rather than only installed. After a clear this is a plain call to an EOA.
	to := a.addr
	return &shape{
		to:       &to,
		value:    big.NewInt(0),
		envelope: envSetCode,
		auth:     []types.SetCodeAuthorization{auth},
	}, nil
}

// zeroizeShape alternates between the two ways a slot can go back to zero, because on
// this tree zero is not a value but an absence: storing zero DELETES the leaf.
//
//	same transaction    initcode writes a batch of slots, then stores zero over all of
//	                    them. Whole stems go from full to empty inside one transaction.
//	across transactions one transaction writes a non-zero slot in the writer contract,
//	                    a later one zeroes it — the leaf is deleted from a tree that has
//	                    already been committed.
//
// Slots straddle 64 on purpose. Below it they live in the account's header stem, next to
// its basic data and code leaves, so the stem must survive the deletion; at or above it
// each group has its own stem, and removing the last leaf collapses it.
func (w *world) zeroizeShape() *shape {
	w.zeroizeTurn++
	if w.zeroizeTurn%2 == 0 {
		return &shape{value: big.NewInt(0), data: txkit.ZeroizeInitCode(txkit.ZeroizeSlots())}
	}

	to := w.writer
	sh := &shape{to: &to, value: big.NewInt(0)}
	// Zero one of the slots we have already written, if there is one; otherwise write a
	// fresh one to zero later. The cap keeps the live set from growing without bound.
	if len(w.liveSlots) > 0 && (len(w.liveSlots) > 64 || w.zeroizeTurn%4 == 1) {
		key := w.liveSlots[0]
		w.liveSlots = w.liveSlots[1:]
		sh.data = append(key.Bytes(), common.Hash{}.Bytes()...) // key, value=0 -> delete
		return sh
	}
	key := txkit.SlotKey(uint64(w.zeroizeTurn))
	if w.zeroizeTurn%3 == 0 {
		key = txkit.SlotKey(uint64(w.zeroizeTurn) % 64) // land in the header stem sometimes
	}
	w.liveSlots = append(w.liveSlots, key)
	var val common.Hash
	val[31] = 0xff
	sh.data = append(key.Bytes(), val.Bytes()...)
	return sh
}

func total(stats map[string]int) int {
	n := 0
	for _, v := range stats {
		n += v
	}
	return n
}

// summary reports the mix in a fixed order, so a workload that stops producing shows up
// as a count that stops moving rather than a missing key.
func summary(stats map[string]int) string {
	parts := make([]string, 0, len(allWorkloads))
	for _, k := range allWorkloads {
		parts = append(parts, fmt.Sprintf("%s=%d", k, stats[k]))
	}
	return strings.Join(parts, " ")
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
