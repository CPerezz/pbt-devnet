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
	id, err := e.c.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	n, err := e.c.PendingNonceAt(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return &sender{key: key, addr: addr, chainID: id, nonce: n}, nil
}

// fees derives a fee cap from the target's current base fee. geth's RPC rejects any
// transaction whose maximum cost exceeds 1 ether, so the cap is clamped to stay well
// under that -- a diverged branch can carry a base fee high enough to trip it, and the
// resulting rejection looks like the scenario silently doing nothing.
func (s *sender) fees(ctx context.Context, e *el, gas uint64) (tip, feeCap *big.Int, err error) {
	h, err := e.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	tip = big.NewInt(1_000_000_000) // 1 gwei
	feeCap = new(big.Int).Add(tip, new(big.Int).Mul(h.BaseFee, big.NewInt(2)))

	maxSpend := new(big.Int).Div(big.NewInt(500_000_000_000_000_000), new(big.Int).SetUint64(gas)) // 0.5 ether / gas
	if feeCap.Cmp(maxSpend) > 0 {
		feeCap = maxSpend
	}
	if feeCap.Cmp(tip) < 0 {
		tip = new(big.Int).Set(feeCap)
	}
	return tip, feeCap, nil
}

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
	if err := e.c.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, common.Address{}, fmt.Errorf("send to %s: %w", e.name, err)
	}
	s.nonce++
	return tx.Hash(), created, nil
}

// await waits for a receipt on the client the transaction was sent to. A scenario that
// carries on without confirming would put its state on neither branch.
func (s *sender) await(ctx context.Context, e *el, h common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for {
		r, err := e.c.TransactionReceipt(ctx, h)
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
