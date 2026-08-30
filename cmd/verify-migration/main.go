// Command verify-migration is the one-shot judge for a migration devnet run.
//
// It reads the migration-monitor's JSONL stream, the chaos driver's JSONL
// stream, a kurtosis service-log dump directory, and a pins file, and asks
// the live execution clients the few questions only the chain can answer.
// It prints one PASS/FAIL/INCONCLUSIVE line per check (C1..C12) with
// evidence and exits with the number of FAILED checks, so 0 means no
// check failed. An inconclusive check does not fail the run: it reports
// that a genuinely stochastic precondition never occurred, so the run
// proved less than a clean pass. Those ids are repeated in a trailing
// "inconclusive:" line.
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
	summaryPath := flag.String("summary", "", "write a one-page markdown record of the run to this path")
	manifestPath := flag.String("manifest", "", "lap manifest JSON: what the driver did (restart, scenarios, quiesce)")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprint(out, "usage: verify-migration [flags]\n\n"+
			"Every check prints one line: PASS, FAIL, or INCONCLUSIVE. INCONCLUSIVE\n"+
			"means a precondition the chain had to supply by chance never occurred\n"+
			"(no reorg deep enough, no post-fork block from the straddle victim), so\n"+
			"the check neither passed nor failed. The exit code is the number of\n"+
			"FAILED checks only; inconclusive ids are listed in a trailing\n"+
			"\"inconclusive:\" line so an operator can see the run proved less than a\n"+
			"clean pass.\n\n")
		flag.PrintDefaults()
	}
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
		els:          els,
		T:            *binaryTrieTime,
		monitor:      monitor,
		chaos:        chaos,
		logsDir:      *logsDir,
		skipChaos:    *skipChaos,
		smoke:        *smoke,
		summaryPath:  *summaryPath,
		manifestPath: *manifestPath,
		fetch:        httpFetcher(&http.Client{Timeout: 15 * time.Second}),
	}
	if *pinsPath == "" {
		v.pinsErr = fmt.Errorf("no --pins file given")
	} else if raw, err := os.ReadFile(*pinsPath); err != nil {
		v.pinsErr = err
	} else {
		v.pins = parsePins(raw)
	}
	if *manifestPath != "" {
		v.manifest, v.manifestErr = loadManifest(*manifestPath)
	}

	os.Exit(v.Run(context.Background(), os.Stdout))
}

// verdict is a check's outcome. Only verdictFail counts against the
// process exit code: verdictInconclusive means a precondition the chain
// had to supply by chance never occurred (no reorg reached depth >= 10,
// no post-fork block came from the straddle victim, a healed isolation
// left no observable reorg), so the check neither passed nor failed and
// the run proved less than a clean pass.
type verdict int

const (
	verdictPass verdict = iota
	verdictFail
	verdictInconclusive
)

func (r verdict) String() string {
	switch r {
	case verdictPass:
		return "PASS"
	case verdictInconclusive:
		return "INCONCLUSIVE"
	default:
		return "FAIL"
	}
}

// boolVerdict lifts a plain pass/fail check into a verdict, for checks
// that have no stochastic precondition of their own.
func boolVerdict(ok bool) verdict {
	if ok {
		return verdictPass
	}
	return verdictFail
}

// checkResult is one check's outcome, kept after Run so --summary can
// render it alongside the run's other evidence.
type checkResult struct {
	id       string
	verdict  verdict
	evidence string
}

// Run executes every check in order, prints one verdict line each,
// prints a trailing "inconclusive:" line naming any INCONCLUSIVE checks,
// optionally writes the --summary artifact, and returns the number of
// FAILED checks (the process exit code; inconclusive checks do not
// count).
func (v *verifier) Run(ctx context.Context, w io.Writer) int {
	v.resolveBStar(ctx)
	checks := []struct {
		id string
		fn func(context.Context) (verdict, string)
	}{
		{"C1", v.checkC1},
		{"C2", v.checkC2},
		{"C3", v.checkC3},
		{"C4", v.checkC4},
		{"C5", v.checkC5},
		{"C6", v.checkC6},
		{"C7", v.checkC7},
		{"C8", v.checkC8},
		{"C9", v.checkC9},
		{"C10", v.checkC10},
		{"C11", v.checkC11},
		{"C12", v.checkC12},
		{"C13", v.checkC13},
		{"C14", v.checkC14},
		{"C15", v.checkC15},
		{"C16", v.checkC16},
		{"C17", v.checkC17},
		{"C18", v.checkC18},
	}
	var results []checkResult
	for _, c := range checks {
		r, evidence := c.fn(ctx)
		fmt.Fprintf(w, "%s %s: %s\n", r, c.id, evidence)
		results = append(results, checkResult{id: c.id, verdict: r, evidence: evidence})
	}
	failed, inconclusiveIDs := summarizeVerdicts(results)
	if len(inconclusiveIDs) > 0 {
		fmt.Fprintf(w, "inconclusive: %s\n", strings.Join(inconclusiveIDs, ","))
	}
	// A run in which not one check PASSed proved nothing at all; exiting 0
	// on it would let a completely dead evidence pipeline read as green.
	if failed == 0 && !anyPassed(results) {
		fmt.Fprintln(w, "FAIL floor: no check passed; a run that proves nothing is not a green run")
		failed = 1
	}
	if only, ok := v.singleImplementation(); ok {
		fmt.Fprintf(w, "NOTE: single-implementation run (%s only): cross-node checks prove determinism, not spec agreement\n", only)
	}
	if v.summaryPath != "" {
		if err := os.WriteFile(v.summaryPath, []byte(v.renderSummary(results)), 0o644); err != nil {
			fmt.Fprintf(w, "warning: --summary write failed: %v\n", err)
		}
	}
	return failed
}

// summarizeVerdicts counts FAILED checks (the exit code) and collects
// the ids of every INCONCLUSIVE check, in order.
func summarizeVerdicts(results []checkResult) (failed int, inconclusiveIDs []string) {
	for _, r := range results {
		switch r.verdict {
		case verdictFail:
			failed++
		case verdictInconclusive:
			inconclusiveIDs = append(inconclusiveIDs, r.id)
		}
	}
	return failed, inconclusiveIDs
}

// anyPassed reports whether at least one check PASSed.
func anyPassed(results []checkResult) bool {
	for _, r := range results {
		if r.verdict == verdictPass {
			return true
		}
	}
	return false
}

// singleImplementation reports whether every configured EL runs the same
// client implementation - in which case the differential checks compare a
// binary against itself and the summary must say so.
func (v *verifier) singleImplementation() (string, bool) {
	first := ""
	for _, e := range v.els {
		c := clientOf(e.name)
		if c == "" {
			return "", false
		}
		if first == "" {
			first = c
			continue
		}
		if c != first {
			return "", false
		}
	}
	return first, first != ""
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
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	StateRoot  common.Hash    `json:"stateRoot"`
	Timestamp  hexutil.Uint64 `json:"timestamp"`
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

// getBlockByHash fetches by hash, tolerating a null result: unlike
// getBlock (used for canonical-tag/number lookups where an absent block
// is always wrong), a null answer to eth_getBlockByHash on an orphaned
// hash is exactly the expected, healthy outcome.
func (v *verifier) getBlockByHash(ctx context.Context, e el, hash string) (*rpcBlock, error) {
	var blk *rpcBlock
	if err := v.fetch(ctx, e.url, "eth_getBlockByHash", &blk, hash, false); err != nil {
		return nil, fmt.Errorf("%s: %w", e.name, err)
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
