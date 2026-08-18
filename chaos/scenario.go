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

	"github.com/CPerezz/pbt-devnet/hammer/txkit"
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
	doc  string
	// setup runs BEFORE the split, through the majority, so whatever it creates lives
	// on the branch that survives.
	setup func(context.Context, *run) error
	// apply runs DURING the split, through the minority only. Everything it writes is
	// doomed.
	apply func(context.Context, *run) error
	// verify runs after the heal, against every client.
	verify func(context.Context, *run) (string, error)
}

// run carries one scenario's state from setup through verification.
type run struct {
	c        *chaos
	minority *el
	majority []*el
	viaMin   *sender // sends through the minority
	viaMaj   *sender // sends through the majority

	doomed    []common.Address // must be empty/absent afterwards
	survivor  common.Address   // must keep its state afterwards
	code      []byte           // the runtime blob deployed on both sides
	slots     []uint64
	slotIsSet bool // true when the slots must READ BACK non-zero after the heal
}

var scenarios = map[string]*scenario{
	"code-sole": {
		name: "code-sole",
		doc:  "unique bytecode deployed only on the doomed branch; its code-zone chunks have no other owner",
		apply: func(ctx context.Context, r *run) error {
			r.code = txkit.PatternCode(400, 0xa1)
			addr, err := r.deploy(ctx, r.minority, r.viaMin, txkit.DeployCodeAfter(nil, r.code))
			if err != nil {
				return err
			}
			r.doomed = append(r.doomed, addr)
			return nil
		},
		verify: func(ctx context.Context, r *run) (string, error) {
			return r.expectNoCode(ctx, r.doomed)
		},
	},

	"code-shared": {
		name: "code-shared",
		doc:  "the same bytecode exists on the surviving branch; the doomed copy goes but the chunks must stay reachable",
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
		verify: func(ctx context.Context, r *run) (string, error) {
			gone, err := r.expectNoCode(ctx, r.doomed)
			if err != nil {
				return gone, err
			}
			// This is the half that go-ethereum#30 is about: dropping the doomed
			// account must not take the shared chunks with it.
			kept, err := r.expectCode(ctx, r.survivor, r.code)
			return gone + "; " + kept, err
		},
	},

	"delegate": {
		name: "delegate",
		doc:  "7702 delegations set on the doomed branch; the delegation leaf must not survive",
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
			_, err := r.viaMin.await(ctx, r.minority, last, 120*time.Second)
			return err
		},
		verify: func(ctx context.Context, r *run) (string, error) {
			// A cleared delegation means no code at all: back to a plain EOA.
			return r.expectNoCode(ctx, r.doomed)
		},
	},

	"account": {
		name: "account",
		doc:  "fresh accounts funded on the doomed branch; their header stems must go with it",
		apply: func(ctx context.Context, r *run) error {
			var last common.Hash
			for i := 0; i < 5; i++ {
				var addr common.Address
				if _, err := rand.Read(addr[:]); err != nil {
					return err
				}
				var err error
				last, _, err = r.viaMin.send(ctx, r.minority, txReq{
					to: &addr, value: big.NewInt(1_000_000_000_000_000), gas: 60_000,
				})
				if err != nil {
					return err
				}
				r.doomed = append(r.doomed, addr)
			}
			_, err := r.viaMin.await(ctx, r.minority, last, 120*time.Second)
			return err
		},
		verify: func(ctx context.Context, r *run) (string, error) {
			var bad []string
			for _, e := range r.all() {
				for _, a := range r.doomed {
					b, err := e.c.BalanceAt(ctx, a, nil)
					if err != nil {
						return "", err
					}
					if b.Sign() != 0 {
						bad = append(bad, fmt.Sprintf("%s still funds %s (%s wei)", e.name, a.Hex(), b))
					}
				}
			}
			return finding(bad, fmt.Sprintf("%d reorged-out accounts are empty on every client", len(r.doomed)))
		},
	},

	"storage-add": {
		name: "storage-add",
		doc:  "slots written on the doomed branch below and above HEADER_STORAGE_OFFSET; both must vanish",
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
		verify: func(ctx context.Context, r *run) (string, error) {
			return r.expectSlots(ctx, r.survivor, r.slots, false)
		},
	},

	"storage-del": {
		name: "storage-del",
		doc:  "slots that existed before the split are DELETED on the doomed branch; the deletion must be undone",
		setup: func(ctx context.Context, r *run) error {
			r.slots = []uint64{1, 2, 3}
			r.slotIsSet = true
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
		verify: func(ctx context.Context, r *run) (string, error) {
			return r.expectSlots(ctx, r.survivor, r.slots, true)
		},
	},
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

// deployGas sizes a deployment under EIP-8297's two-dimensional gas, where every byte of
// deployed code costs 1530 state gas. A flat allowance silently made every scenario
// unsendable: 3000 bytes alone needs 4.59M, well past the 3M that used to be passed.
func deployGas(codeLen int) uint64 {
	return 500_000 + uint64(codeLen)*1800
}

func (r *run) deploy(ctx context.Context, e *el, s *sender, initcode []byte) (common.Address, error) {
	h, addr, err := s.send(ctx, e, txReq{data: initcode, gas: deployGas(len(initcode))})
	if err != nil {
		return common.Address{}, err
	}
	rec, err := s.await(ctx, e, h, 120*time.Second)
	if err != nil {
		return common.Address{}, err
	}
	if rec.Status != types.ReceiptStatusSuccessful {
		return common.Address{}, fmt.Errorf("deployment reverted on %s", e.name)
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
	_, err := r.viaMin.await(ctx, r.minority, last, 120*time.Second)
	return err
}

func (r *run) expectNoCode(ctx context.Context, addrs []common.Address) (string, error) {
	var bad []string
	for _, e := range r.all() {
		for _, a := range addrs {
			code, err := e.c.CodeAt(ctx, a, nil)
			if err != nil {
				return "", err
			}
			if len(code) > 0 {
				bad = append(bad, fmt.Sprintf("%s still has %d bytes of code at %s", e.name, len(code), a.Hex()))
			}
		}
	}
	return finding(bad, fmt.Sprintf("%d reorged-out accounts have no code on any client", len(addrs)))
}

func (r *run) expectCode(ctx context.Context, addr common.Address, want []byte) (string, error) {
	var bad []string
	for _, e := range r.all() {
		code, err := e.c.CodeAt(ctx, addr, nil)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(code, want) {
			bad = append(bad, fmt.Sprintf("%s has %d bytes at the surviving account %s, wanted %d",
				e.name, len(code), addr.Hex(), len(want)))
		}
	}
	return finding(bad, fmt.Sprintf("the shared %d-byte blob is still readable at %s on every client",
		len(want), addr.Hex()))
}

// expectSlots checks each slot reads back as the seeded value (wantSet) or as zero.
func (r *run) expectSlots(ctx context.Context, addr common.Address, slots []uint64, wantSet bool) (string, error) {
	var bad []string
	for _, e := range r.all() {
		for _, s := range slots {
			got, err := e.c.StorageAt(ctx, addr, txkit.SlotKey(s), nil)
			if err != nil {
				return "", err
			}
			want := common.Hash{}
			if wantSet {
				want = txkit.SlotKey(s + 1) // what SeedCode wrote
			}
			if !bytes.Equal(got, want.Bytes()) {
				bad = append(bad, fmt.Sprintf("%s reads slot %d as %s, wanted %s",
					e.name, s, common.BytesToHash(got).Hex(), want.Hex()))
			}
		}
	}
	what := "are zero again"
	if wantSet {
		what = "are back to their pre-split values"
	}
	return finding(bad, fmt.Sprintf("slots %v %s on every client", slots, what))
}

// finding turns a list of disagreements into either a passing note or an error. An error
// here is the interesting outcome: it means two clients disagree about state after a
// reorg, which is the whole reason this devnet exists.
func finding(bad []string, ok string) (string, error) {
	if len(bad) == 0 {
		return ok, nil
	}
	return "", fmt.Errorf("%s", strings.Join(bad, "; "))
}
