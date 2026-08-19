package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/txkit"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// A scenario puts specific state on a branch that is about to be reorged out, and then
// asks every client the same question about that state once the branch is gone. The
// interesting answer is disagreement: one client keeping the doomed branch's state while
// another drops it is exactly the divergence two implementations are here to expose.
type scenario struct {
	name string
	// setup runs BEFORE the split, through the majority, so whatever it creates lives
	// on the branch that survives.
	setup func(context.Context, *run) error
	// apply runs DURING the split, through the minority only. Everything it writes is
	// doomed.
	apply func(context.Context, *run) error
	// effect answers one question -- is the doomed change visible on this client at this
	// height? -- and the runner asks it three times: it must be TRUE on the minority
	// before the heal, FALSE on the majority at that same moment, and FALSE on every
	// client at the anchor afterwards.
	//
	// One predicate rather than an "assert absent" is what makes the scenarios honest.
	// Absence is trivially true when nothing was written, and it cannot express
	// storage-del, where the doomed change IS an
	// absence and the surviving branch is the one holding values.
	//
	// A nil height means latest.
	effect func(context.Context, *run, *el, *big.Int) (bool, error)
	// survives is optional: state that must still be READABLE after the heal. Only
	// code-shared needs it, and it is the half of go-ethereum#30 that says chunks stay
	// when a surviving account still holds them.
	survives func(context.Context, *run, *el, *big.Int) error
}

// run carries one scenario's state from setup through verification.
type run struct {
	c        *chaos
	minority *el
	majority []*el
	viaMin   *sender // sends through the minority
	viaMaj   *sender // sends through the majority

	doomed   []common.Address // written on the branch that dies
	survivor common.Address   // written before the split, must outlive it
	code     []byte           // the runtime blob deployed on both sides
	slots    []uint64

	// at is the height the final check is evaluated at: the majority's tip, recorded
	// before the heal and four blocks below its head.
	//
	// This has to be a fixed block, not "latest". A reorged-out transaction is still
	// valid -- same sender, same nonce -- so it returns to the mempool and is re-mined on
	// the surviving branch within a block or two, recreating the contract at the same
	// address. Checking at head therefore measures how fast the pool re-broadcast, not
	// whether the tree dropped the abandoned branch's writes.
	at *big.Int
}

var scenarios = map[string]*scenario{
	"code-sole": {
		name: "code-sole",
		apply: func(ctx context.Context, r *run) error {
			r.code = txkit.PatternCode(400, 0xa1)
			addr, err := r.deploy(ctx, r.minority, r.viaMin, txkit.DeployCodeAfter(nil, r.code))
			if err != nil {
				return err
			}
			r.doomed = append(r.doomed, addr)
			return nil
		},
		effect: hasCode,
	},

	"code-shared": {
		name: "code-shared",
		setup: func(ctx context.Context, r *run) error {
			// A different fill would make this a second code-sole. The whole point is
			// that both accounts hold the IDENTICAL blob.
			r.code = txkit.PatternCode(400, 0xb2)
			addr, err := r.deploy(ctx, r.majority[0], r.viaMaj, txkit.DeployCodeAfter(nil, r.code))
			if err != nil {
				return err
			}
			r.survivor = addr
			return nil
		},
		apply: func(ctx context.Context, r *run) error {
			addr, err := r.deploy(ctx, r.minority, r.viaMin, txkit.DeployCodeAfter(nil, r.code))
			if err != nil {
				return err
			}
			r.doomed = append(r.doomed, addr)
			return nil
		},
		effect: hasCode,
		// Dropping the doomed account must not take the shared chunks with it.
		survives: func(ctx context.Context, r *run, e *el, at *big.Int) error {
			code, err := e.c.CodeAt(ctx, r.survivor, at)
			if err != nil {
				return err
			}
			if !bytes.Equal(code, r.code) {
				return fmt.Errorf("%s has %d bytes at the surviving account %s, wanted %d",
					e.name, len(code), r.survivor.Hex(), len(r.code))
			}
			return nil
		},
	},

	"delegate": {
		name: "delegate",
		setup: func(ctx context.Context, r *run) error {
			r.code = txkit.PatternCode(300, 0xc3)
			addr, err := r.deploy(ctx, r.majority[0], r.viaMaj, txkit.DeployCodeAfter(nil, r.code))
			if err != nil {
				return err
			}
			r.survivor = addr
			return nil
		},
		apply: func(ctx context.Context, r *run) error {
			var last common.Hash
			for i := 0; i < 3; i++ {
				key, err := crypto.GenerateKey()
				if err != nil {
					return err
				}
				authority := crypto.PubkeyToAddress(key.PublicKey)
				// A fresh account has nonce 0, so the authorization needs no lookup --
				// and a lookup against a partitioned node could not be trusted anyway.
				auth, err := types.SignSetCode(key, types.SetCodeAuthorization{
					ChainID: *uint256.MustFromBig(r.viaMin.chainID),
					Address: r.survivor,
					Nonce:   0,
				})
				if err != nil {
					return err
				}
				last, _, err = r.viaMin.send(ctx, r.minority, txReq{to: &authority, auth: []types.SetCodeAuthorization{auth}})
				if err != nil {
					return err
				}
				r.doomed = append(r.doomed, authority)
			}
			// Nonces are sequential from one sender, so the last receipt implies the rest.
			return r.viaMin.awaitOK(ctx, r.minority, last, 120*time.Second)
		},
		// A delegation shows up as code: 0xef0100 followed by the target.
		effect: hasCode,
	},

	"account": {
		name: "account",
		apply: func(ctx context.Context, r *run) error {
			var last common.Hash
			for i := 0; i < 5; i++ {
				var addr common.Address
				if _, err := rand.Read(addr[:]); err != nil {
					return err
				}
				var err error
				// Funding a fresh address CREATES an account, and under EIP-8297 that
				// is 207,391 state gas on top of the 21,000 intrinsic.
				last, _, err = r.viaMin.send(ctx, r.minority, txReq{
					to: &addr, value: big.NewInt(1_000_000_000_000_000), gas: 300_000,
				})
				if err != nil {
					return err
				}
				r.doomed = append(r.doomed, addr)
			}
			return r.viaMin.awaitOK(ctx, r.minority, last, 120*time.Second)
		},
		effect: func(ctx context.Context, r *run, e *el, at *big.Int) (bool, error) {
			for _, a := range r.doomed {
				b, err := e.c.BalanceAt(ctx, a, at)
				if err != nil {
					return false, err
				}
				if b.Sign() != 0 {
					return true, nil
				}
			}
			return false, nil
		},
	},

	"storage-add": {
		name: "storage-add",
		setup: func(ctx context.Context, r *run) error {
			// 7 shares the header stem, 70 gets a dedicated storage stem. One scenario
			// covers both sides of the boundary because they are different code paths.
			r.slots = []uint64{7, 70}
			addr, err := r.deploy(ctx, r.majority[0], r.viaMaj, txkit.DeployCodeAfter(nil, txkit.WriterRuntime()))
			if err != nil {
				return err
			}
			r.survivor = addr
			return nil
		},
		apply: func(ctx context.Context, r *run) error {
			return r.writeAll(ctx, r.survivor, r.slots, func(s uint64) common.Hash {
				return txkit.SlotKey(s + 1)
			})
		},
		// The doomed change is the slots being set.
		effect: func(ctx context.Context, r *run, e *el, at *big.Int) (bool, error) {
			return anySlot(ctx, e, r.survivor, r.slots, at, false)
		},
	},

	"storage-del": {
		name: "storage-del",
		setup: func(ctx context.Context, r *run) error {
			r.slots = []uint64{1, 2, 3}
			// Seed in the initcode, so the values are already on the surviving branch
			// before anything is partitioned.
			init := txkit.DeployCodeAfter(txkit.SeedCode(r.slots), txkit.WriterRuntime())
			addr, err := r.deploy(ctx, r.majority[0], r.viaMaj, init)
			if err != nil {
				return err
			}
			r.survivor = addr
			return nil
		},
		apply: func(ctx context.Context, r *run) error {
			// Zero IS absence in this tree, so this is a leaf deletion, and reorging it
			// out means resurrecting a value rather than dropping one.
			return r.writeAll(ctx, r.survivor, r.slots, func(uint64) common.Hash {
				return common.Hash{}
			})
		},
		// Here the doomed change is the slots being GONE, which is why effect has to be a
		// predicate: on the surviving branch these read non-zero and that is correct.
		effect: func(ctx context.Context, r *run, e *el, at *big.Int) (bool, error) {
			return anySlot(ctx, e, r.survivor, r.slots, at, true)
		},
	},
}

// hasCode reports whether any doomed address holds code, which covers a deployment and a
// 7702 delegation alike.
func hasCode(ctx context.Context, r *run, e *el, at *big.Int) (bool, error) {
	for _, a := range r.doomed {
		code, err := e.c.CodeAt(ctx, a, at)
		if err != nil {
			return false, err
		}
		if len(code) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// anySlot reports whether any of the slots is zero (wantZero) or non-zero.
func anySlot(ctx context.Context, e *el, addr common.Address, slots []uint64, at *big.Int, wantZero bool) (bool, error) {
	for _, s := range slots {
		got, err := e.c.StorageAt(ctx, addr, txkit.SlotKey(s), at)
		if err != nil {
			return false, err
		}
		isZero := common.BytesToHash(got) == (common.Hash{})
		if isZero == wantZero {
			return true, nil
		}
	}
	return false, nil
}

func scenarioNames() []string {
	out := make([]string, 0, len(scenarios))
	for k := range scenarios {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r *run) all() []*el { return append([]*el{r.minority}, r.majority...) }

// deployGas sizes a deployment under EIP-8297's two-dimensional gas.
//
// EIP-8297 charges 1530 state gas per byte of new state; this multiplies by 1800, because
// the argument is the INITCODE length and the gas has to cover the account the deployment
// creates plus the chunking overhead, not just the runtime that ends up stored.
//
// 260k covers creating the account itself before a single byte of code is written: a fresh
// account is about 207,391 state gas, and a deployment always makes one. The constant stays as small as it can be: cost is
// feeCap*gas and the fee cap is a fixed budget divided by the gas, so an oversized limit
// prices the transaction out on a chain whose base fee has risen.
func deployGas(codeLen int) uint64 {
	return 260_000 + uint64(codeLen)*1800
}

func (r *run) deploy(ctx context.Context, e *el, s *sender, initcode []byte) (common.Address, error) {
	h, addr, err := s.send(ctx, e, txReq{data: initcode, gas: deployGas(len(initcode))})
	if err != nil {
		return common.Address{}, err
	}
	if err := s.awaitOK(ctx, e, h, 120*time.Second); err != nil {
		return common.Address{}, err
	}
	return addr, nil
}

// writeAll calls the writer contract once per slot -- calldata is key || value -- and
// confirms only the last, since one sender's nonces are sequential.
func (r *run) writeAll(ctx context.Context, to common.Address, slots []uint64, val func(uint64) common.Hash) error {
	var last common.Hash
	for _, s := range slots {
		data := append(txkit.SlotKey(s).Bytes(), val(s).Bytes()...)
		h, _, err := r.viaMin.send(ctx, r.minority, txReq{to: &to, data: data, gas: 200_000})
		if err != nil {
			return err
		}
		last = h
	}
	return r.viaMin.awaitOK(ctx, r.minority, last, 120*time.Second)
}

// describeEffect reports which clients see the doomed change, for an error message.
func (r *run) describeEffect(ctx context.Context, sc *scenario, els []*el, at *big.Int) string {
	var yes, no []string
	for _, e := range els {
		on, err := sc.effect(ctx, r, e, at)
		switch {
		case err != nil:
			no = append(no, e.name+"=?")
		case on:
			yes = append(yes, e.name)
		default:
			no = append(no, e.name)
		}
	}
	sort.Strings(yes)
	sort.Strings(no)
	return fmt.Sprintf("visible on [%s], absent on [%s]", strings.Join(yes, " "), strings.Join(no, " "))
}
