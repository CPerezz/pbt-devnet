package migmon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// ErrNoIntrospection is returned by a client for a node whose execution
// implementation exposes no migration introspection. It is not a failure of
// the node: the caller degrades to what standard RPC can answer - heads and
// canonical hashes - and says so in its evidence.
var ErrNoIntrospection = errors.New("this client exposes no migration introspection")

// Header is the part of a block every consumer here needs: identity, and
// the timestamp the fork is measured against.
type Header struct {
	Number uint64
	Hash   string
	Time   uint64
}

// Client is one execution client, seen through the two surfaces the
// migration tooling uses: standard RPC, which every implementation
// answers, and migration introspection, which only an implementation that
// migrates in place can answer.
//
// The split is the seam for a second client: add an implementation of the
// introspection half and everything above it - the monitor's findings, the
// gate's done-detection, the verifier's expectations - works unchanged.
type Client interface {
	Name() string
	// Introspects reports whether this client can answer migration
	// progress at all. It is part of the interface rather than inferred
	// from a concrete type so that every caller - and every test double -
	// states the capability instead of guessing it.
	Introspects() bool
	// Introspection. Returns ErrNoIntrospection when Introspects is false.
	Progress(ctx context.Context) (json.RawMessage, error)
	ShadowRoot(ctx context.Context, hash string) (string, error)
	// Standard RPC, answered by every client.
	HeadNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, height uint64) (*Header, error)
	HeaderByTag(ctx context.Context, tag string) (*Header, error)
}

// ClientType returns the execution implementation named in a service name.
// The package names services el-<index>-<execution>-<consensus>, which is
// the only place the wire tells us what we are talking to.
func ClientType(service string) string {
	parts := strings.Split(service, "-")
	if len(parts) >= 3 && parts[0] == "el" {
		return parts[2]
	}
	return ""
}

// NewClient returns a client for a service name and RPC URL. An
// implementation with no known introspection adapter still gets a working
// standard-RPC client, with the introspection half reporting
// ErrNoIntrospection so callers can degrade deliberately instead of
// mistaking silence for agreement.
func NewClient(service, url string) Client {
	rpc := &rpcClient{name: service, url: url, http: &http.Client{Timeout: 10 * time.Second}}
	switch ClientType(service) {
	case "geth":
		return rpc
	default:
		return noIntrospection{rpc}
	}
}

// HasIntrospection reports whether a client can answer migration progress.
func HasIntrospection(c Client) bool { return c.Introspects() }

// noIntrospection wraps a standard-RPC client for an implementation whose
// migration state is not reachable over RPC.
type noIntrospection struct{ Client }

func (n noIntrospection) Introspects() bool { return false }

func (n noIntrospection) Progress(context.Context) (json.RawMessage, error) {
	return nil, ErrNoIntrospection
}

func (n noIntrospection) ShadowRoot(context.Context, string) (string, error) {
	return "", ErrNoIntrospection
}

// rpcClient speaks plain JSON-RPC. The migration tooling never calls the
// engine API, so there is no JWT and no second port to track.
type rpcClient struct {
	name  string
	url   string
	http  *http.Client
	reqID atomic.Uint64
}

func (c *rpcClient) Name() string      { return c.name }
func (c *rpcClient) Introspects() bool { return true }

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

func (c *rpcClient) call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: c.reqID.Add(1)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.name, method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", c.name, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: http %d: %s", c.name, method, resp.StatusCode, cli.Trim(raw, 400))
	}

	var parsed rpcResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("%s %s: decode envelope: %w (body %s)", c.name, method, err, cli.Trim(raw, 400))
	}
	if parsed.Error != nil {
		return fmt.Errorf("%s %s: %w", c.name, method, parsed.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("%s %s: decode result: %w (result %s)", c.name, method, err, cli.Trim(parsed.Result, 400))
	}
	return nil
}

// rpcHeader is the wire shape of the header fields we read.
type rpcHeader struct {
	Number    hexutil.Uint64 `json:"number"`
	Hash      common.Hash    `json:"hash"`
	Timestamp hexutil.Uint64 `json:"timestamp"`
}

func (c *rpcClient) HeaderByNumber(ctx context.Context, height uint64) (*Header, error) {
	return c.header(ctx, hexutil.EncodeUint64(height))
}

// HeaderByTag reads a named head: "finalized" is how the tooling learns
// that a fork block has settled, without opening a consensus-layer API.
func (c *rpcClient) HeaderByTag(ctx context.Context, tag string) (*Header, error) {
	return c.header(ctx, tag)
}

func (c *rpcClient) header(ctx context.Context, param string) (*Header, error) {
	var h *rpcHeader
	if err := c.call(ctx, "eth_getBlockByNumber", &h, param, false); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, nil // a legal absence: the tag has no block yet
	}
	return &Header{Number: uint64(h.Number), Hash: h.Hash.Hex(), Time: uint64(h.Timestamp)}, nil
}

func (c *rpcClient) HeadNumber(ctx context.Context) (uint64, error) {
	var out hexutil.Uint64
	if err := c.call(ctx, "eth_blockNumber", &out); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

func (c *rpcClient) Progress(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.call(ctx, "debug_migrationProgress", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ShadowRoot returns the recorded shadow root for a block, or "" for a
// legal null: the record has not been written yet, which is never
// divergence on its own.
func (c *rpcClient) ShadowRoot(ctx context.Context, hash string) (string, error) {
	var out *common.Hash
	if err := c.call(ctx, "debug_shadowStateRoot", &out, hash); err != nil {
		return "", err
	}
	if out == nil {
		return "", nil
	}
	return out.Hex(), nil
}
