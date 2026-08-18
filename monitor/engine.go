package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Node is one execution client: an authenticated engine endpoint and a plain
// JSON-RPC endpoint. The two are separate ports in geth and carry different auth,
// so they are kept as separate clients rather than one.
type Node struct {
	Name      string
	EngineURL string
	RPCURL    string

	jwtSecret []byte
	http      *http.Client
	reqID     atomic.Uint64
}

func NewNode(name, engineURL, rpcURL, jwtSecretPath string) (*Node, error) {
	raw, err := os.ReadFile(jwtSecretPath)
	if err != nil {
		return nil, fmt.Errorf("read jwt secret: %w", err)
	}
	hexed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	secret := make([]byte, len(hexed)/2)
	if _, err := fmt.Sscanf(hexed, "%x", &secret); err != nil {
		return nil, fmt.Errorf("decode jwt secret: %w", err)
	}
	return &Node{
		Name:      name,
		EngineURL: engineURL,
		RPCURL:    rpcURL,
		jwtSecret: secret,
		// Generous timeout: a payload carrying a large block access list plus a
		// cold binary-tree read path can be slow, and a client-side timeout would
		// look exactly like a divergence.
		http: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// authToken mints the HS256 token geth's authrpc expects. The `iat` claim must be
// within 60s of the server's clock or the request is rejected, so it is minted per
// call rather than cached.
func (n *Node) authToken() (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iat": time.Now().Unix(),
	})
	return tok.SignedString(n.jwtSecret)
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

// Engine calls an authenticated engine_* method and unmarshals the result.
func (n *Node) Engine(ctx context.Context, method string, out any, params ...any) error {
	tok, err := n.authToken()
	if err != nil {
		return err
	}
	return n.call(ctx, n.EngineURL, map[string]string{"Authorization": "Bearer " + tok}, method, out, params...)
}

// RPC calls an unauthenticated eth_*/debug_* method.
func (n *Node) RPC(ctx context.Context, method string, out any, params ...any) error {
	return n.call(ctx, n.RPCURL, nil, method, out, params...)
}

func (n *Node) call(ctx context.Context, url string, headers map[string]string, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      n.reqID.Add(1),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := n.http.Do(req)
	if err != nil {
		// Failing to reach the node at all is an outage, not a disagreement — but a
		// node that accepts the connection and then stops answering is a bug we want,
		// so timeouts are deliberately NOT classified as transport failures.
		if isUnreachable(err) {
			return &TransportError{node: n.Name, method: method, err: err}
		}
		return fmt.Errorf("%s %s: %w", n.Name, method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if isUnreachable(err) {
			return &TransportError{node: n.Name, method: method, err: err}
		}
		return fmt.Errorf("%s %s: read body: %w", n.Name, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: http %d: %s", n.Name, method, resp.StatusCode, truncate(raw, 400))
	}

	var parsed rpcResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("%s %s: decode envelope: %w (body %s)", n.Name, method, err, truncate(raw, 400))
	}
	if parsed.Error != nil {
		return fmt.Errorf("%s %s: %w", n.Name, method, parsed.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("%s %s: decode result: %w (result %s)", n.Name, method, err, truncate(parsed.Result, 400))
	}
	return nil
}

// WaitReady blocks until the node answers eth_chainId, or the context expires.
func (n *Node) WaitReady(ctx context.Context) error {
	var chainID string
	for {
		err := n.RPC(ctx, "eth_chainId", &chainID)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s never became ready: %w", n.Name, err)
		case <-time.After(time.Second):
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// TransportError marks a failure to *reach* a node, as distinct from an answer the
// node gave. Only this type is treated as an outage.
//
// The distinction has to be structural. Matching substrings against the error text
// is unsafe here because a rejection message embeds geth's own validationError, so a
// genuine INVALID whose validation error happens to mention "EOF" — the shape an RLP
// or block-access-list decode divergence takes — would be dismissed as an outage and
// never counted. Errors are therefore tagged where they are produced.
type TransportError struct {
	node   string
	method string
	err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("%s %s: unreachable: %v", e.node, e.method, e.err)
}
func (e *TransportError) Unwrap() error { return e.err }

// IsTransportError reports whether err is an availability problem rather than a
// disagreement between the nodes.
func IsTransportError(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

// isUnreachable distinguishes "the node is not there" from "the node went quiet".
// A restarting container refuses connections or fails DNS; it does not accept a
// connection and then hang. So a timeout means a node that is up and stuck, which is
// a finding, not an outage.
func isUnreachable(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Anything else at the transport layer — refused, reset, no such host, no route.
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}
