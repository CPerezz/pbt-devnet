package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/CPerezz/pbt-devnet/internal/cli"
)

// node is one execution client's plain JSON-RPC endpoint. Unlike pbtmonitor
// this daemon never calls the engine API, so there is no JWT and no second
// port to track.
type node struct {
	Name string
	URL  string

	http  *http.Client
	reqID atomic.Uint64
}

func newNode(name, url string) *node {
	return &node{Name: name, URL: url, http: &http.Client{Timeout: 10 * time.Second}}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      uint64 `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call issues one JSON-RPC request and decodes its result into out (nil to
// discard). Shape mirrors cmd/pbtmonitor's Node.call, minus the auth header
// this daemon never needs.
func (n *node) call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: n.reqID.Add(1)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", n.Name, method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", n.Name, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: http %d: %s", n.Name, method, resp.StatusCode, cli.Trim(raw, 400))
	}

	var parsed rpcResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("%s %s: decode envelope: %w (body %s)", n.Name, method, err, cli.Trim(raw, 400))
	}
	if parsed.Error != nil {
		return fmt.Errorf("%s %s: %w", n.Name, method, parsed.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("%s %s: decode result: %w (result %s)", n.Name, method, err, cli.Trim(parsed.Result, 400))
	}
	return nil
}

// rpcHeader is the subset of eth_getBlockByNumber's result the monitor needs:
// identity (number, hash) and the timestamp that b* is measured against.
type rpcHeader struct {
	Number    hexutil.Uint64 `json:"number"`
	Hash      common.Hash    `json:"hash"`
	Timestamp hexutil.Uint64 `json:"timestamp"`
}

func blockNumberHex(n uint64) string { return hexutil.EncodeUint64(n) }

func getHeader(ctx context.Context, n *node, height uint64) (*rpcHeader, error) {
	var h rpcHeader
	if err := n.call(ctx, "eth_getBlockByNumber", &h, blockNumberHex(height), false); err != nil {
		return nil, err
	}
	return &h, nil
}

func getBlockNumber(ctx context.Context, n *node) (uint64, error) {
	var out hexutil.Uint64
	if err := n.call(ctx, "eth_blockNumber", &out); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

func getMigrationProgress(ctx context.Context, n *node) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := n.call(ctx, "debug_migrationProgress", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// getShadowStateRoot returns the shadow root for hash, or "" for a legal
// null (the record has not been written yet — never treat as divergence).
func getShadowStateRoot(ctx context.Context, n *node, hash common.Hash) (string, error) {
	var out *common.Hash
	if err := n.call(ctx, "debug_shadowStateRoot", &out, hash); err != nil {
		return "", err
	}
	if out == nil {
		return "", nil
	}
	return out.Hex(), nil
}
