// Straddle state injection: known conflicting writes on both sides of the
// fork-spanning partition, so the verifier can assert the rewind outcome.
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

// injector holds one funded sender per side; one key on both sides would
// fork its own nonce stream.
type injector struct {
	victimURL   string // the straddle victim's own RPC
	majorityURL string // a never-disrupted node's RPC
	victimKey   *ecdsa.PrivateKey
	majorityKey *ecdsa.PrivateKey

	contract common.Address
}

// Fee caps sit far above any devnet basefee: side basefees drift while split.
var (
	injTip = big.NewInt(3_000_000_000)  // 3 gwei
	injCap = big.NewInt(60_000_000_000) // 60 gwei
)

// injectionSlot gets conflicting values from both sides; injectionSlotMajority
// is written by the majority alone.
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

// deploy places the WriterRuntime target (stores calldata word 1 at slot word 0)
// on the canonical chain before the straddle opens. Seed leaves slots 3,4 non-zero.
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

// splitWrites fires the conflicting writes while the partition is open. The
// victim block hash is captured before the heal makes it unreachable by number.
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

	// Majority first: its values must win.
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

	// Victim side: the conflicting write.
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
// code. Nonce is read from the target side each call since the sides diverge.
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
