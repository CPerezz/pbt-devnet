package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// sender signs and submits transactions to ONE execution client. Scenarios need that:
// a transaction meant for the doomed branch has to enter through the minority's RPC and
// nowhere else, or it lands on both branches and the scenario proves nothing.
//
// Nonces are tracked locally rather than re-read each time, because during a partition
// the minority's view is the only one that counts and it is the one we are writing to.
type sender struct {
	key     *ecdsa.PrivateKey
	addr    common.Address
	chainID *big.Int
	nonce   uint64
}

func newSender(ctx context.Context, hexKey string, e *el) (*sender, error) {
	key, err := crypto.HexToECDSA(trimHex(hexKey))
	if err != nil {
		return nil, fmt.Errorf("bad key: %w", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	cctx0, ccancel0 := elCtx(ctx)
	id, err := e.c.ChainID(cctx0)
	ccancel0()
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	cctx, ccancel := elCtx(ctx)
	n, err := e.c.PendingNonceAt(cctx, addr)
	ccancel()
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return &sender{key: key, addr: addr, chainID: id, nonce: n}, nil
}

// fees picks the highest fee cap the RPC will accept, rather than a multiple of the
// current base fee.
//
// A multiple does not work here, and fails in a way that looks like the scenario doing
// nothing: the fee is derived from the chain BEFORE the partition, but the partition is
// what moves the price. Cut off with its share of the validators and the full transaction
// load still pointed at it, the minority's blocks run full and its base fee climbs away
// from the pre-partition value within a few blocks -- long past 2x anything.
//
// Signing high costs nothing: under EIP-1559 the sender pays base fee plus tip, and the
// cap is only a ceiling. The one real limit is the node's own RPC guard, which rejects a
// transaction whose maximum cost exceeds 1 ether, so stay just under that.
func (s *sender) fees(ctx context.Context, e *el, gas uint64) (tip, feeCap *big.Int, err error) {
	cctx, ccancel := elCtx(ctx)
	h, err := e.c.HeaderByNumber(cctx, nil)
	ccancel()
	if err != nil {
		return nil, nil, err
	}
	// Outbid the load generators. Blocks on this devnet run 99.9% full -- the hammer and
	// spamoor between them fill 200M gas -- and builders order by effective tip, so a
	// scenario transaction at the hammer's own 1 gwei is tied for last and simply never
	// gets in. That failure is silent: the send succeeds, no receipt ever arrives, and
	// the scenario times out looking like a partition problem.
	//
	// 50 gwei sits far above the 1-2 gwei the generators use, and still costs a fraction
	// of an ether at these gas figures.
	tip = big.NewInt(50_000_000_000) // 50 gwei
	feeCap = new(big.Int).Div(feeBudgetWei, new(big.Int).SetUint64(gas))

	// The budget is a ceiling, never a target to be raised past. Repeated partitions push the base fee up (each leaves a backlog), so this
	// is reached in practice, not in theory.
	if feeCap.Cmp(h.BaseFee) <= 0 {
		return nil, nil, fmt.Errorf(
			"base fee on %s is %s wei; %d gas cannot be paid for within the node's 1 ether "+
				"cap (max %s wei/gas). Let the chain settle, or lower the load",
			e.name, h.BaseFee, gas, feeCap)
	}
	if feeCap.Cmp(tip) < 0 {
		tip = new(big.Int).Set(feeCap)
	}
	return tip, feeCap, nil
}

// feeBudgetWei is the most a single transaction may cost in total. Nodes reject anything
// whose gasFeeCap*gas exceeds 1 ether, so stay under it.
var feeBudgetWei = big.NewInt(900_000_000_000_000_000)

type txReq struct {
	to    *common.Address
	data  []byte
	value *big.Int
	gas   uint64
	auth  []types.SetCodeAuthorization
}

// send submits one transaction and returns its hash plus, for a creation, the address
// the contract will live at.
func (s *sender) send(ctx context.Context, e *el, r txReq) (common.Hash, common.Address, error) {
	gas := r.gas
	if gas == 0 {
		gas = 1_000_000
	}
	tip, feeCap, err := s.fees(ctx, e, gas)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}
	value := r.value
	if value == nil {
		value = big.NewInt(0)
	}

	var inner types.TxData
	if len(r.auth) > 0 {
		if r.to == nil {
			return common.Hash{}, common.Address{}, fmt.Errorf("a set-code transaction cannot be a creation")
		}
		inner = &types.SetCodeTx{
			ChainID: uint256.MustFromBig(s.chainID), Nonce: s.nonce, To: *r.to,
			Gas: gas, GasTipCap: uint256.MustFromBig(tip), GasFeeCap: uint256.MustFromBig(feeCap),
			Value: uint256.MustFromBig(value), Data: r.data, AuthList: r.auth,
		}
	} else {
		inner = &types.DynamicFeeTx{
			ChainID: s.chainID, Nonce: s.nonce, To: r.to, Gas: gas,
			GasTipCap: tip, GasFeeCap: feeCap, Value: value, Data: r.data,
		}
	}

	tx, err := types.SignNewTx(s.key, types.LatestSignerForChainID(s.chainID), inner)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}
	created := common.Address{}
	if r.to == nil {
		created = crypto.CreateAddress(s.addr, s.nonce)
	}
	cctx, ccancel := elCtx(ctx)
	err = e.c.SendTransaction(cctx, tx)
	ccancel()
	if err != nil {
		return common.Hash{}, common.Address{}, fmt.Errorf("send to %s: %w", e.name, err)
	}
	s.nonce++
	return tx.Hash(), created, nil
}

// awaitOK waits for a receipt and insists the transaction actually succeeded.
//
// Discarding the status lets a scenario pass without doing anything: each asserts an
// ABSENCE afterwards -- no code, zero balance, slots cleared -- which is
// trivially true when the write never landed. A revert, an out-of-gas or a rejected 7702
// authorization all read as success.
func (s *sender) awaitOK(ctx context.Context, e *el, h common.Hash, timeout time.Duration) error {
	rec, err := s.await(ctx, e, h, timeout)
	if err != nil {
		return err
	}
	if rec.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transaction %s reverted on %s (gas used %d of the limit)",
			short(h), e.name, rec.GasUsed)
	}
	return nil
}

// await waits for a receipt on the client the transaction was sent to. A scenario that
// carries on without confirming would put its state on neither branch.
func (s *sender) await(ctx context.Context, e *el, h common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for {
		cctx, ccancel := elCtx(ctx)
		r, err := e.c.TransactionReceipt(cctx, h)
		ccancel()
		if err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no receipt for %s on %s after %s", short(h), e.name, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func trimHex(s string) string {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}
