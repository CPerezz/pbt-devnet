// Command hammer generates transactions aimed at what EIP-8297 changed, rather than
// at throughput. A generic spammer moves ether between existing accounts, which
// touches one leaf each and says little about a new state commitment.
//
//	fanout   fresh recipients        -> new header stems
//	storage  spread storage slots    -> the storage zone and its 66-byte keys
//	codedup  identical code from
//	         several senders         -> code-zone leaves SHARED between accounts, the
//	                                    property that makes rollback impossible
//	destruct factory deploys a child
//	         with code, then destroys
//	         it in the same tx       -> code-zone leaves nothing reclaims
//
// Only EIP-1559 transactions so far; 7702, access lists and legacy are not covered.
//
// All bytecode is straight-line with no jumps, so it needs no compiler.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// The standard local-development keys, matching the genesis alloc.
var devKeys = []string{
	"ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	"59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
	"5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a",
	"7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
	"47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a",
	"8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba",
}

type sender struct {
	key   *ecdsa.PrivateKey
	addr  common.Address
	nonce uint64
}

func main() {
	var (
		rpcURLs   multiFlag
		chainID   = flag.Int64("chainid", 1337, "chain id")
		rate      = flag.Duration("interval", 400*time.Millisecond, "time between batches")
		batch     = flag.Int("batch", 4, "transactions per batch")
		slots     = flag.Int("slots-per-tx", 24, "storage slots written per storage tx")
		codeSize  = flag.Int("code-size", 12_000, "runtime code size for the codedup workload")
		only      = flag.String("only", "", "run only this workload (fanout|storage|codedup|destruct)")
		duration  = flag.Duration("for", 0, "stop after this long (0 = run forever)")
		gasTipCap = flag.Int64("tip", 1_000_000_000, "max priority fee per gas, wei")
	)
	flag.Var(&rpcURLs, "rpc", "execution rpc endpoint (repeatable; every tx is sent to all of them)")
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

	signer := types.LatestSignerForChainID(big.NewInt(*chainID))
	senders := make([]*sender, 0, len(devKeys))
	for _, hexKey := range devKeys {
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
	sharedCode := patternCode(*codeSize, 0x5f)

	head, err := clients[0].HeaderByNumber(ctx, nil)
	if err != nil {
		fatal("head: %v", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(1_000_000_000)
	}
	gasFeeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(4)), big.NewInt(*gasTipCap))

	all := []string{"fanout", "storage", "codedup", "destruct"}
	workloads := all
	if *only != "" {
		if !slices.Contains(all, *only) {
			fatal("--only %q is not a workload; choose from %v", *only, all)
		}
		workloads = []string{*only}
	}

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

			tx, err := buildTx(ctx, clients, kind, s, gasFeeCap, big.NewInt(*gasTipCap), big.NewInt(*chainID), *slots, sharedCode)
			if err != nil {
				slog.Warn("build failed", "workload", kind, "err", err)
				continue
			}
			signed, err := types.SignTx(tx, signer, s.key)
			if err != nil {
				slog.Warn("sign failed", "err", err)
				continue
			}

			// Send to EVERY node, not one of them. The nodes are unpeered, so a
			// transaction submitted to only one leaves the other with a nonce gap; the
			// gapped transaction sits in the queued subpool, is capped at 64 per
			// account, and the surplus is dropped — with no error returned. Two thirds
			// of the load used to disappear this way. Broadcasting is what devp2p
			// gossip would have done for us.
			accepted := false
			for _, cl := range clients {
				if err := cl.SendTransaction(ctx, signed); err != nil {
					slog.Warn("send failed", "workload", kind, "from", s.addr, "nonce", s.nonce, "err", err)
					continue
				}
				accepted = true
			}
			if !accepted {
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
			slog.Info("sent", "fanout", stats["fanout"], "storage", stats["storage"],
				"codedup", stats["codedup"], "destruct", stats["destruct"])
			checkNonceDrift(ctx, clients, senders)
			gasFeeCap = refreshFeeCap(ctx, clients[0], gasFeeCap, *gasTipCap)
		}
	}
	slog.Info("hammer done", "fanout", stats["fanout"], "storage", stats["storage"],
		"codedup", stats["codedup"], "destruct", stats["destruct"])
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

func buildTx(ctx context.Context, clients []*ethclient.Client, kind string, s *sender, gasFeeCap, gasTipCap, chainID *big.Int, slots int, sharedCode []byte) (*types.Transaction, error) {
	var (
		to   *common.Address
		data []byte
		val  = big.NewInt(0)
	)
	switch kind {
	case "fanout":
		// A brand-new random recipient every time, so every one of these adds a
		// header stem the tree has never held.
		var addr common.Address
		if _, err := rand.Read(addr[:]); err != nil {
			return nil, err
		}
		to, val = &addr, big.NewInt(1)

	case "storage":
		// Creation whose initcode writes `slots` scattered storage slots and returns
		// empty code: a fresh account plus a wide scatter of storage-zone leaves.
		data = storageInitCode(slots)

	case "codedup":
		// Same runtime code from a rotating set of senders: identical code hash,
		// shared code-zone leaves.
		data = deployCode(sharedCode)

	case "destruct":
		// A modest child so the workload stays cheap next to codedup; the point is
		// that code exists at all when the account disappears.
		data = factoryDestructInitCode(s.addr, 2048)

	default:
		return nil, fmt.Errorf("unknown workload %q", kind)
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
	call := ethereum.CallMsg{
		From:      s.addr,
		To:        to,
		Value:     val,
		Data:      data,
		GasFeeCap: gasFeeCap,
		GasTipCap: gasTipCap,
	}
	gas, err := estimateEverywhere(ctx, clients, call)
	if err != nil {
		return nil, err
	}
	// 1% only. A wide margin would paper over exactly what we want to see: if the
	// estimate does not match execution, that gap is a finding about the gas model,
	// not something to absorb silently.
	gas = gas + gas/100

	return types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     s.nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Gas:       gas,
		To:        to,
		Value:     val,
		Data:      data,
	}), nil
}

// storageInitCode returns initcode that performs `n` unrolled SSTOREs and then
// returns zero-length code. Straight-line, no jumps: PUSH32 value, PUSH32 key,
// SSTORE, repeated, then PUSH1 0 PUSH1 0 RETURN.
func storageInitCode(n int) []byte {
	code := make([]byte, 0, n*67+5)
	for i := 0; i < n; i++ {
		var key, val common.Hash
		// 24 random leading bytes keep successive slots far apart in the tree
		// instead of sharing a stem.
		if _, err := rand.Read(key[:24]); err != nil {
			panic(err)
		}
		key[31] = byte(i)
		val[31] = byte(i + 1)

		code = append(code, 0x7f) // PUSH32 value
		code = append(code, val[:]...)
		code = append(code, 0x7f) // PUSH32 key
		code = append(code, key[:]...)
		code = append(code, 0x55) // SSTORE
	}
	code = append(code, 0x60, 0x00, 0x60, 0x00, 0xf3) // PUSH1 0 PUSH1 0 RETURN
	return code
}

// deployCode wraps a runtime blob in the standard CODECOPY/RETURN preamble.
//
//	PUSH2 len | PUSH1 14 | PUSH1 0 | CODECOPY | PUSH2 len | PUSH1 0 | RETURN | blob
//
// The preamble is exactly 14 (0x0e) bytes, which is the blob's offset in the code.
func deployCode(runtime []byte) []byte {
	n := len(runtime)
	if n > 0xffff {
		panic("runtime blob too large for a PUSH2 length")
	}
	out := []byte{
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x60, 0x0e, // PUSH1 14 (code offset of the blob)
		0x60, 0x00, // PUSH1 0  (memory destination)
		0x39,                       // CODECOPY
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x60, 0x00, // PUSH1 0
		0xf3, // RETURN
	}
	return append(out, runtime...)
}

// factoryDestructInitCode returns initcode that deploys a child WITH REAL CODE and
// then destroys it in the same transaction.
//
// The obvious shape — `PUSH20 addr; SELFDESTRUCT` as the creation transaction's own
// initcode — deploys nothing, because SELFDESTRUCT halts before any RETURN. It cost
// 215,814 gas against 210,588 for a bare empty CREATE, i.e. it wrote no code at all,
// so the workload never produced the orphaned code-zone leaves it was meant to.
//
// A factory fixes that: the child returns `codeSize` bytes of code, so code-zone
// leaves are written, and the parent then calls it so it self-destructs. Post
// EIP-6780 SELFDESTRUCT only deletes when the account was created in the same
// transaction — which is exactly this case — so the account goes away while its code
// chunks remain, with nothing reference-counting them.
//
// Parent layout (31-byte prefix, then the child initcode it copies out of itself):
//
//	PUSH2 len | PUSH2 31 | PUSH1 0 | CODECOPY      copy child initcode to memory
//	PUSH2 len | PUSH1 0  | PUSH1 0 | CREATE        deploy it -> address
//	PUSH1 0 ×5 | DUP6 | GAS | CALL | STOP          call it -> SELFDESTRUCT
func factoryDestructInitCode(beneficiary common.Address, codeSize int) []byte {
	// Child runtime: destroy on any call, followed by inert filler that exists only
	// to occupy code-zone leaves.
	childRuntime := append([]byte{0x73}, beneficiary.Bytes()...) // PUSH20 beneficiary
	childRuntime = append(childRuntime, 0xff)                    // SELFDESTRUCT
	for len(childRuntime) < codeSize {
		childRuntime = append(childRuntime, 0xfe) // INVALID: never reached, never executed
	}
	childInit := deployCode(childRuntime)

	n := len(childInit)
	const prefixLen = 31
	out := []byte{
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x61, 0x00, prefixLen, // PUSH2 31  (offset of childInit in this code)
		0x60, 0x00, // PUSH1 0   (memory destination)
		0x39,                       // CODECOPY
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x60, 0x00, // PUSH1 0   (memory offset)
		0x60, 0x00, // PUSH1 0   (value)
		0xf0,       // CREATE     -> [addr]
		0x60, 0x00, // retLength
		0x60, 0x00, // retOffset
		0x60, 0x00, // argsLength
		0x60, 0x00, // argsOffset
		0x60, 0x00, // value
		0x85, // DUP6 -> copy addr to the top
		0x5a, // GAS
		0xf1, // CALL
		0x00, // STOP
	}
	if len(out) != prefixLen {
		panic(fmt.Sprintf("factory prefix is %d bytes, the PUSH2 offset says %d", len(out), prefixLen))
	}
	return append(out, childInit...)
}

// patternCode builds a deterministic runtime blob that is safe to CALL: byte 0 is
// STOP, so execution halts immediately and the rest is inert data.
//
// The terminator has to be FIRST. Filling with PUSH0 and putting a STOP at the end
// produced code that overflowed the 1024-slot stack after ~1024 bytes and reverted,
// burning all forwarded gas — so the contracts could be deployed but never called,
// and the code-zone read path went untested.
//
// fill selects the body, which decides the code hash: the same fill from different
// senders shares code-zone leaves, a different fill does not.
func patternCode(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	if n > 0 {
		out[0] = 0x00 // STOP
	}
	return out
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
