// Straddle state injection: engineered, conflicting writes on both islands
// of the fork-spanning partition. Ambient hammer traffic gives the doomed
// branch arbitrary state; these writes give it KNOWN state - one contract,
// known slots, a distinct value per island - so the acceptance verifier can
// assert the rewind's outcome (majority values canonical everywhere, the
// victim-island block gone) instead of hoping random traffic exercised it.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/txkit"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// injector holds one funded sender per island. Separate keys per island are
// load-bearing: one key used on both sides would fork its own nonce stream
// and turn every assertion into a race over which island's tx survives.
type injector struct {
	victimURL   string // the straddle victim's own RPC: the only door into its island
	majorityURL string // a never-disrupted node's RPC: always the majority island
	victimKey   *ecdsa.PrivateKey
	majorityKey *ecdsa.PrivateKey

	contract common.Address
}

// Fee headroom: the two islands' basefees drift apart while split, so the
// caps are set far above any devnet basefee rather than estimated - an
// underpriced injection failing to mine would read as a rewind bug.
var (
	injTip = big.NewInt(3_000_000_000)  // 3 gwei
	injCap = big.NewInt(60_000_000_000) // 60 gwei
)

// injectionSlot is the storage slot both islands write conflicting values
// to; injectionSlotMajority is written by the majority alone, so its value
// must survive regardless of any victim-side salvage.
var (
	injectionSlot         = txkit.SlotKey(1)
	injectionSlotMajority = txkit.SlotKey(2)
	victimValue           = common.HexToHash("0xaa000000000000000000000000000000000000000000000000000000000000aa")
	majorityValue         = common.HexToHash("0xbb000000000000000000000000000000000000000000000000000000000000bb")
	majorityOnlyValue     = common.HexToHash("0xcc000000000000000000000000000000000000000000000000000000000000cc")
)

func newInjector(victimURL, majorityURL string, keys []*ecdsa.PrivateKey) (*injector, error) {
	if len(keys) < 2 {
		return nil, fmt.Errorf("need 2 --key senders (one per island), got %d", len(keys))
	}
	return &injector{
		victimURL:   victimURL,
		majorityURL: majorityURL,
		victimKey:   keys[0],
		majorityKey: keys[1],
	}, nil
}

// deploy places the shared target contract on the canonical chain before
// the straddle opens: WriterRuntime stores calldata word 1 at the slot
// named by word 0, so every later injection is a plain call with
// slot||value calldata - state changes that EXECUTE, not bytecode mailed
// to a contract that ignores its input. A seed prefix leaves two non-zero
// slots behind for delete-shaped writes. Runs against the majority node:
// pre-straddle the network is whole, so this is simply the canonical chain.
func (in *injector) deploy(ctx context.Context, log *migmon.Log) error {
	c, err := ethclient.DialContext(ctx, in.majorityURL)
	if err != nil {
		return err
	}
	defer c.Close()
	init := txkit.DeployCodeAfter(txkit.SeedCode([]uint64{3, 4}), txkit.WriterRuntime())
	tx, err := in.send(ctx, c, in.majorityKey, nil, init)
	if err != nil {
		return err
	}
	rcpt, err := waitReceipt(ctx, c, tx.Hash(), 60*time.Second)
	if err != nil {
		return fmt.Errorf("deploy receipt: %w", err)
	}
	in.contract = rcpt.ContractAddress
	log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: fmt.Sprintf("injection contract deployed at %s", in.contract.Hex())})
	return nil
}

// writeCall is the WriterRuntime calldata for "store val at slot".
func writeCall(slot, val common.Hash) []byte {
	return append(slot.Bytes(), val.Bytes()...)
}

// splitWrites fires the conflicting writes while the partition is open:
// the same slot set to a different value through each island's own door,
// plus one majority-only slot. Victim-side inclusion evidence (the island
// block hash) is captured from inside the island, before the heal makes
// that block unreachable by number.
func (in *injector) splitWrites(ctx context.Context, log *migmon.Log) {
	if (in.contract == common.Address{}) {
		return // deploy failed; already warned
	}
	emit := func(inj migmon.Injection) {
		raw, err := json.Marshal(inj)
		if err != nil {
			return
		}
		log.Emit(migmon.Event{Kind: migmon.EvInject, Node: inj.Side, Detail: fmt.Sprintf("slot %s = %s via %s island", inj.Slot, inj.Value, inj.Side), Raw: raw})
	}

	// Majority island first: its values are the ones that must win.
	if c, err := ethclient.DialContext(ctx, in.majorityURL); err == nil {
		for slot, val := range map[common.Hash]common.Hash{
			injectionSlot:         majorityValue,
			injectionSlotMajority: majorityOnlyValue,
		} {
			tx, err := in.send(ctx, c, in.majorityKey, &in.contract, writeCall(slot, val))
			if err != nil {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "majority injection: " + err.Error()})
				continue
			}
			if _, err := waitReceipt(ctx, c, tx.Hash(), 45*time.Second); err != nil {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "majority injection receipt: " + err.Error()})
				continue
			}
			emit(migmon.Injection{Contract: in.contract.Hex(), Slot: slot.Hex(), Value: val.Hex(), Side: "majority", TxHash: tx.Hash().Hex()})
		}
		c.Close()
	} else {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "majority island dial: " + err.Error()})
	}

	// Victim island: the conflicting write on the doomed branch.
	if c, err := ethclient.DialContext(ctx, in.victimURL); err == nil {
		tx, err := in.send(ctx, c, in.victimKey, &in.contract, writeCall(injectionSlot, victimValue))
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "victim injection: " + err.Error()})
		} else if rcpt, err := waitReceipt(ctx, c, tx.Hash(), 45*time.Second); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "victim injection receipt: " + err.Error()})
		} else {
			emit(migmon.Injection{Contract: in.contract.Hex(), Slot: injectionSlot.Hex(), Value: victimValue.Hex(),
				Side: "victim", TxHash: tx.Hash().Hex(), IslandBlock: rcpt.BlockHash.Hex()})
		}
		c.Close()
	} else {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "victim island dial: " + err.Error()})
	}
}

// send signs and submits one dynamic-fee tx; to == nil deploys data as init
// code. Nonce comes fresh from the target island's own view each call - the
// two islands legitimately diverge on it once txs land on one side only.
func (in *injector) send(ctx context.Context, c *ethclient.Client, key *ecdsa.PrivateKey, to *common.Address, data []byte) (*types.Transaction, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := c.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	chainID, err := c.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	gas := uint64(600_000)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: injTip, GasFeeCap: injCap,
		Gas: gas, To: to, Data: data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		return nil, err
	}
	return signed, c.SendTransaction(ctx, signed)
}

func waitReceipt(ctx context.Context, c *ethclient.Client, h common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rcpt, err := c.TransactionReceipt(ctx, h); err == nil && rcpt != nil {
			return rcpt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("no receipt for %s within %s", h.Hex(), timeout)
}
