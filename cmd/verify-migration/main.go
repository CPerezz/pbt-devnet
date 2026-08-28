// Command verify-migration is the one-shot judge for a migration devnet run.
//
// It reads the migration-monitor's JSONL stream, the chaos driver's JSONL
// stream, a kurtosis service-log dump directory, and a pins file, and asks
// the live execution clients the few questions only the chain can answer.
// It prints one PASS/FAIL line per check (C1..C8) with evidence and exits
// with the number of failed checks, so 0 means the run passed.
//
// b* is the first canonical block whose header timestamp is >= T
// (--binary-trie-time). It is taken from the monitor's bstar events and
// cross-checked against an RPC backwalk; a disagreement fails every check
// that needs it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// el is one execution client the verifier may question over JSON-RPC.
type el struct{ name, url string }

// elFlags collects repeatable --el name=url entries.
type elFlags []el

func (e *elFlags) String() string {
	parts := make([]string, len(*e))
	for i, x := range *e {
		parts[i] = x.name + "=" + x.url
	}
	return strings.Join(parts, ",")
}

func (e *elFlags) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("--el wants name=url, got %q", v)
	}
	*e = append(*e, el{name: name, url: url})
	return nil
}

func main() {
	var els elFlags
	flag.Var(&els, "el", "name=rpcURL of one execution client (repeatable, at least one)")
	monitorJSONL := flag.String("monitor-jsonl", "", "migration-monitor JSONL stream (required)")
	chaosJSONL := flag.String("chaos-jsonl", "", "migration-chaos JSONL stream (optional)")
	logsDir := flag.String("logs-dir", "", "kurtosis service-log dump directory")
	pinsPath := flag.String("pins", "", "pins file: simple 'key: value' lines")
	binaryTrieTime := flag.Uint64("binary-trie-time", 0, "unix time T of the binary-trie fork (required)")
	skipChaos := flag.Bool("skip-chaos", false, "chaos was not run: C3 is skipped, C6 drops its window math")
	smoke := flag.Bool("smoke", false, "single-node smoke run: relaxed C4/C6/C7 thresholds")
	flag.Parse()

	usage := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		flag.Usage()
		os.Exit(2)
	}
	if len(els) == 0 {
		usage("at least one --el name=url is required")
	}
	if *monitorJSONL == "" {
		usage("--monitor-jsonl is required")
	}
	if *binaryTrieTime == 0 {
		usage("--binary-trie-time is required")
	}

	monitor, err := readEvents(*monitorJSONL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading monitor jsonl: %v\n", err)
		os.Exit(2)
	}
	var chaos []migmon.Event
	if *chaosJSONL != "" {
		if chaos, err = readEvents(*chaosJSONL); err != nil {
			fmt.Fprintf(os.Stderr, "reading chaos jsonl: %v\n", err)
			os.Exit(2)
		}
	}

	v := &verifier{
		els:       els,
		T:         *binaryTrieTime,
		monitor:   monitor,
		chaos:     chaos,
		logsDir:   *logsDir,
		skipChaos: *skipChaos,
		smoke:     *smoke,
		fetch:     httpFetcher(&http.Client{Timeout: 15 * time.Second}),
	}
	if *pinsPath == "" {
		v.pinsErr = fmt.Errorf("no --pins file given")
	} else if raw, err := os.ReadFile(*pinsPath); err != nil {
		v.pinsErr = err
	} else {
		v.pins = parsePins(raw)
	}

	os.Exit(v.Run(context.Background(), os.Stdout))
}

// Run executes every check in order, prints one verdict line each, and
// returns the number of failures (the process exit code).
func (v *verifier) Run(ctx context.Context, w io.Writer) int {
	v.resolveBStar(ctx)
	checks := []struct {
		id string
		fn func(context.Context) (bool, string)
	}{
		{"C1", v.checkC1},
		{"C2", v.checkC2},
		{"C3", v.checkC3},
		{"C4", v.checkC4},
		{"C5", v.checkC5},
		{"C6", v.checkC6},
		{"C7", v.checkC7},
		{"C8", v.checkC8},
	}
	failed := 0
	for _, c := range checks {
		pass, evidence := c.fn(ctx)
		verdict := "PASS"
		if !pass {
			verdict = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "%s %s: %s\n", verdict, c.id, evidence)
	}
	return failed
}

// fetcher performs one JSON-RPC call against url and decodes result into out.
// It is a function so tests can substitute canned answers for a live node.
type fetcher func(ctx context.Context, url, method string, out any, params ...any) error

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

// httpFetcher is the live implementation: the same plain JSON-RPC-over-HTTP
// shape cmd/pbtmonitor uses, without its engine-API half.
func httpFetcher(client *http.Client) fetcher {
	var id uint64
	return func(ctx context.Context, url, method string, out any, params ...any) error {
		if params == nil {
			params = []any{}
		}
		id++
		body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: id})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("%s: read body: %w", method, err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, cli.Trim(raw, 400))
		}
		var parsed rpcResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("%s: decode envelope: %w (body %s)", method, err, cli.Trim(raw, 400))
		}
		if parsed.Error != nil {
			return fmt.Errorf("%s: %w", method, parsed.Error)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(parsed.Result, out); err != nil {
			return fmt.Errorf("%s: decode result: %w (result %s)", method, err, cli.Trim(parsed.Result, 400))
		}
		return nil
	}
}

// rpcBlock is the subset of eth_getBlockByNumber the checks compare on,
// mirroring cmd/pbtmonitor's shape.
type rpcBlock struct {
	Number    hexutil.Uint64 `json:"number"`
	Hash      common.Hash    `json:"hash"`
	StateRoot common.Hash    `json:"stateRoot"`
	Timestamp hexutil.Uint64 `json:"timestamp"`
}

func (v *verifier) getBlock(ctx context.Context, e el, numberOrTag string) (*rpcBlock, error) {
	var blk *rpcBlock
	if err := v.fetch(ctx, e.url, "eth_getBlockByNumber", &blk, numberOrTag, false); err != nil {
		return nil, fmt.Errorf("%s: %w", e.name, err)
	}
	if blk == nil {
		return nil, fmt.Errorf("%s has no block %s", e.name, numberOrTag)
	}
	return blk, nil
}

func (v *verifier) txCount(ctx context.Context, e el, n uint64) (uint64, error) {
	var out *hexutil.Uint64
	if err := v.fetch(ctx, e.url, "eth_getBlockTransactionCountByNumber", &out, hexutil.EncodeUint64(n)); err != nil {
		return 0, fmt.Errorf("%s: %w", e.name, err)
	}
	if out == nil {
		return 0, fmt.Errorf("%s has no block %d", e.name, n)
	}
	return uint64(*out), nil
}
