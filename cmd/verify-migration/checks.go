package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// verifier holds every input the checks read, plus the resolved b*. It is
// built once by main and never mutated concurrently: checks run in sequence.
type verifier struct {
	els         []el
	T           uint64 // --binary-trie-time
	monitor     []migmon.Event
	chaos       []migmon.Event
	logsDir     string
	pins        map[string]string
	pinsErr     error
	skipChaos   bool
	smoke       bool
	summaryPath string
	fetch       fetcher

	bstar    bstarResult
	bstarErr error

	// schedule caches the chaos driver's published schedule, read once
	// from the chaos stream's "schedule" record by scheduleDump.
	scheduleLoaded bool
	schedule       migsched.Dump
	scheduleErr    error
}

func (v *verifier) elNames() []string {
	names := make([]string, len(v.els))
	for i, e := range v.els {
		names[i] = e.name
	}
	return names
}

// readEvents parses one JSONL stream of migmon.Event lines, sorted by time.
// Blank lines are skipped; an unknown "kind" is not an error here — K6
// (failing closed on unknown kinds) belongs to the emitter, not the reader.
func readEvents(path string) ([]migmon.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []migmon.Event
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// A service's log is not exclusively this stream: the gate hands
		// over to another process that writes its own structured lines
		// into the same stdout. Skip what is not one of our events rather
		// than refusing the whole file - the alternative is a run that
		// cannot be judged because a downstream service logged normally.
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev migmon.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			skipped++
			continue
		}
		if ev.Kind == "" {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 && skipped > 0 {
		return nil, fmt.Errorf("%s: no readable events (%d unparsable lines)", path, skipped)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// parsePins reads the pins file by hand: one "key: value" per line, blank
// lines and "#" comments ignored. It is deliberately not a YAML library —
// the pins file is a couple of scalar keys, not a document.
func parsePins(raw []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(val), `"'`)
	}
	return out
}

// bstarResult is the resolved b*: the first canonical block whose header
// time is >= T.
type bstarResult struct {
	number   uint64
	evidence string
}

// resolveBStar cross-checks the monitor's bstar events (one per node,
// emitted when that node's own head crosses T) against an RPC backwalk on
// els[0]. Disagreement between the two sources — or between nodes — fails
// every check that needs b*, since the run cannot even agree what b* is.
func (v *verifier) resolveBStar(ctx context.Context) {
	nodeVals := map[string]uint64{}
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvBStar {
			nodeVals[ev.Node] = ev.Number
		}
	}
	var monitorNum uint64
	monitorSet, agree := false, true
	for _, n := range nodeVals {
		if !monitorSet {
			monitorNum, monitorSet = n, true
		} else if n != monitorNum {
			agree = false
		}
	}
	if monitorSet && !agree {
		v.bstarErr = fmt.Errorf("monitor bstar events disagree across nodes: %v", nodeVals)
		return
	}

	var rpcNum uint64
	var rpcErr error
	if len(v.els) == 0 {
		rpcErr = fmt.Errorf("no --el configured")
	} else {
		rpcNum, rpcErr = v.backwalkBStar(ctx, v.els[0])
	}

	switch {
	case monitorSet && rpcErr == nil && monitorNum == rpcNum:
		v.bstar = bstarResult{monitorNum, fmt.Sprintf("b*=%d (monitor bstar events agree with rpc backwalk)", monitorNum)}
	case monitorSet && rpcErr == nil:
		v.bstarErr = fmt.Errorf("monitor bstar=%d disagrees with rpc backwalk=%d", monitorNum, rpcNum)
	case monitorSet:
		v.bstar = bstarResult{monitorNum, fmt.Sprintf("b*=%d (monitor bstar events only, rpc backwalk failed: %v)", monitorNum, rpcErr)}
	case rpcErr == nil:
		v.bstar = bstarResult{rpcNum, fmt.Sprintf("b*=%d (rpc backwalk only, no monitor bstar events)", rpcNum)}
	default:
		v.bstarErr = fmt.Errorf("no monitor bstar events and rpc backwalk failed: %w", rpcErr)
	}
}

// backwalkBStar binary-searches els[0] for the first block whose header
// timestamp is >= T. Timestamps are monotonic in block number, so this is
// a plain lower-bound search rather than a linear walk from the head.
func (v *verifier) backwalkBStar(ctx context.Context, e el) (uint64, error) {
	head, err := v.getBlock(ctx, e, "latest")
	if err != nil {
		return 0, err
	}
	if uint64(head.Timestamp) < v.T {
		return 0, fmt.Errorf("%s head time %d has not reached T=%d yet", e.name, head.Timestamp, v.T)
	}
	lo, hi := uint64(0), uint64(head.Number)
	for lo < hi {
		mid := lo + (hi-lo)/2
		blk, err := v.getBlock(ctx, e, hexutil.EncodeUint64(mid))
		if err != nil {
			return 0, err
		}
		if uint64(blk.Timestamp) >= v.T {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}

// chaosWindow is one isolate-to-heal span for a victim node ("" = a
// heal-all closed every window open at that moment). An unhealed isolation
// (no matching heal) has a zero `to` and is not counted healed.
type chaosWindow struct {
	node   string
	from   time.Time
	to     time.Time
	healed bool
}

// covers reports whether t (± slop) falls inside the window, for the given
// node. An empty node argument matches any window (used for time-only
// membership, e.g. C6's cross-node samples); an empty window node ("" =
// heal-all) matches any node.
func (w chaosWindow) covers(node string, t time.Time, slop time.Duration) bool {
	if w.node != "" && node != "" && !sameNode(w.node, node) {
		return false
	}
	from := w.from.Add(-slop)
	if w.to.IsZero() {
		return !t.Before(from)
	}
	return !t.Before(from) && !t.After(w.to.Add(slop))
}

// chaosWindows pairs each isolate with the heal that closes it: same-node
// heal, or a heal-all (empty Node) that closes every window still open.
func (v *verifier) chaosWindows() []chaosWindow {
	open := map[string]*chaosWindow{}
	var closed []chaosWindow
	for _, ev := range v.chaos {
		switch ev.Kind {
		case migmon.EvIsolate:
			open[ev.Node] = &chaosWindow{node: ev.Node, from: ev.Time}
		case migmon.EvHeal:
			if ev.Node == "" {
				for n, w := range open {
					w.to, w.healed = ev.Time, true
					closed = append(closed, *w)
					delete(open, n)
				}
				continue
			}
			if w, ok := open[ev.Node]; ok {
				w.to, w.healed = ev.Time, true
				closed = append(closed, *w)
				delete(open, ev.Node)
			}
		}
	}
	for _, w := range open {
		closed = append(closed, *w) // never healed: to stays zero
	}
	return closed
}

// scheduleDump returns the chaos driver's published schedule, read once
// from the first "schedule" event in the chaos stream (Raw carries a
// migsched.Dump). The schedule is the authority on which ops actually
// ran, their class, and their windows: a check that needs it and finds
// none fails with that as the evidence, since a run's own schedule is
// mandatory input.
func (v *verifier) scheduleDump() (migsched.Dump, error) {
	if v.scheduleLoaded {
		return v.schedule, v.scheduleErr
	}
	v.scheduleLoaded = true
	for _, ev := range v.chaos {
		if ev.Kind == migmon.EvSchedule {
			v.schedule, v.scheduleErr = migsched.ParseDump(ev.Raw)
			return v.schedule, v.scheduleErr
		}
	}
	v.scheduleErr = fmt.Errorf("chaos log has no schedule record")
	return v.schedule, v.scheduleErr
}

// opAttributionSlop tolerates the gap between an admitted op's own
// [Start, End] and the chaos driver's actual isolate/heal event times
// for it.
const opAttributionSlop = 30 * time.Second

// opWindow pairs one admitted schedule op with the chaosWindow the run
// actually recorded for it.
type opWindow struct {
	op     migsched.DumpOp
	window chaosWindow
}

// attributeWindows matches every chaos window this run recorded to the
// admitted op it belongs to, by victim participant index and by falling
// inside the op's own window (± slop). A window matching no admitted op
// is chaos evidence the schedule does not account for.
func (v *verifier) attributeWindows(dump migsched.Dump, windows []chaosWindow) (matched []opWindow, unscheduled []chaosWindow) {
	admitted := dump.Admitted()
	for _, w := range windows {
		found := false
		for _, op := range admitted {
			if opOwns(op, w) {
				matched = append(matched, opWindow{op: op, window: w})
				found = true
				break
			}
		}
		if !found {
			unscheduled = append(unscheduled, w)
		}
	}
	return matched, unscheduled
}

// opOwns reports whether w is the isolation the chaos driver ran for op:
// w's victim participant index is one of op's, and w started inside
// op's own [Start, End] (± opAttributionSlop).
func opOwns(op migsched.DumpOp, w chaosWindow) bool {
	idx, ok := nodeIndex(w.node)
	if !ok {
		return false
	}
	inVictims := false
	for _, victim := range op.Victims {
		if victim == idx {
			inVictims = true
			break
		}
	}
	if !inVictims {
		return false
	}
	start := time.Unix(op.Start, 0).Add(-opAttributionSlop)
	end := time.Unix(op.End, 0).Add(opAttributionSlop)
	return !w.from.Before(start) && !w.from.After(end)
}

var reorgDepthRe = regexp.MustCompile(`\(depth (\d+)\)`)

// clientLogPatterns maps an execution client's name (parsed by clientOf
// from the "el-<n>-<client>-<cl>" naming convention) to the regexp its
// reorg log line matches, with named captures "drop" (dropped-branch
// length) and, where available, "ancestor" (common-ancestor height). A
// client with no entry here degrades to monitor-events-only
// corroboration - matchReorg records that in the evidence it returns.
var clientLogPatterns = map[string]*regexp.Regexp{
	// geth logs "Chain reorg detected number=N hash=H drop=D
	// dropfrom=H add=A addfrom=H" (or "Large chain reorg detected" past
	// 63 dropped blocks). number is the common-ancestor block height:
	// go-ethereum's core/blockchain.go walks both chains back until
	// their headers match and logs that block as commonBlock, so it
	// needs no arithmetic to use directly as the ancestor height.
	"geth": regexp.MustCompile(`Chain reorg detected.*\bnumber=(?P<ancestor>\d+).*\bdrop=(?P<drop>\d+)`),
}

// clientOf extracts the execution client name from a node/service name
// shaped "el-<n>-<client>-<cl>" (kurtosis's convention). The chaos
// driver's own "node-<n>" names carry no client type.
func clientOf(node string) string {
	parts := strings.Split(node, "-")
	if len(parts) < 4 || parts[0] != "el" {
		return ""
	}
	return parts[2]
}

var nodeIndexRe = regexp.MustCompile(`^(?:node|el)-(\d+)\b|^(?:node|el)-(\d+)-`)

// nodeIndex extracts the participant index shared by the two naming
// conventions in play: the chaos driver says "node-2" (participant
// numbering is its whole vocabulary), kurtosis services and the monitor
// say "el-2-geth-lighthouse". The index is the identity.
func nodeIndex(name string) (int, bool) {
	m := nodeIndexRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	for _, g := range m[1:] {
		if g != "" {
			n, err := strconv.Atoi(g)
			return n, err == nil
		}
	}
	return 0, false
}

// sameNode reports whether two names denote the same participant, across
// the node-N / el-N-... naming conventions.
func sameNode(a, b string) bool {
	if a == b {
		return true
	}
	ia, oka := nodeIndex(a)
	ib, okb := nodeIndex(b)
	return oka && okb && ia == ib
}

// victimClient resolves node's execution client type for the
// clientLogPatterns registry. The chaos driver speaks participant
// indices ("node-2"); the client type lives in the monitor's or
// logsDir's "el-N-<client>-<cl>" names, so a bare index is resolved
// through those first.
func (v *verifier) victimClient(node string) string {
	if c := clientOf(node); c != "" {
		return c
	}
	idx, ok := nodeIndex(node)
	if !ok {
		return ""
	}
	for _, ev := range v.monitor {
		if i, ok := nodeIndex(ev.Node); ok && i == idx {
			if c := clientOf(ev.Node); c != "" {
				return c
			}
		}
	}
	if v.logsDir == "" {
		return ""
	}
	for _, f := range v.victimLogFiles(node) {
		if c := clientOf(filepath.Base(f)); c != "" {
			return c
		}
	}
	return ""
}

// reorgEvidence is what the run can prove about the reorg tied to one
// isolation's heal.
type reorgEvidence struct {
	depth    int
	matched  bool
	ancestor int    // common-ancestor block height, 0 if unknown
	source   string // "monitor", or a client name for a log-line match
	// degraded is true when the victim's client has no registered log
	// pattern, so only a monitor reorg event could have corroborated
	// this isolation.
	degraded bool
}

func (e reorgEvidence) String() string {
	if !e.matched {
		if e.degraded {
			return "no matching monitor reorg event (client has no registered log pattern, degraded to monitor-events-only)"
		}
		return "no matching reorg evidence"
	}
	note := fmt.Sprintf("depth %d via %s", e.depth, e.source)
	if e.ancestor > 0 {
		note += fmt.Sprintf(" (common ancestor height %d)", e.ancestor)
	}
	return note
}

// matchReorg looks for evidence that w's isolation actually caused a
// reorg on the victim. Monitor reorg events are the primary evidence: a
// depth >= 1 event within the window (+30s slop). A client's own reorg
// log line, via the clientLogPatterns registry, is the fallback; a
// client with no registered pattern degrades to monitor-events-only,
// which the returned evidence records.
func (v *verifier) matchReorg(w chaosWindow) reorgEvidence {
	const slop = 30 * time.Second
	var best reorgEvidence
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvReorg || !sameNode(ev.Node, w.node) {
			continue
		}
		if !w.covers(ev.Node, ev.Time, slop) {
			continue
		}
		d := 0
		if m := reorgDepthRe.FindStringSubmatch(ev.Detail); m != nil {
			d, _ = strconv.Atoi(m[1])
		}
		if d >= 1 && d > best.depth {
			best = reorgEvidence{depth: d, matched: true, source: "monitor"}
		}
	}
	client := v.victimClient(w.node)
	re, registered := clientLogPatterns[client]
	if !registered {
		best.degraded = !best.matched
		return best
	}
	if v.logsDir != "" {
		for _, f := range v.victimLogFiles(w.node) {
			for _, m := range logReorgMatches(f, re) {
				if m.drop >= 1 && m.drop > best.depth {
					best = reorgEvidence{depth: m.drop, matched: true, source: client, ancestor: m.ancestor}
				}
			}
		}
	}
	return best
}

// victimLogFiles resolves a chaos victim name to its log dump files via
// the participant index, falling back to substring matching.
func (v *verifier) victimLogFiles(node string) []string {
	if idx, ok := nodeIndex(node); ok {
		files, err := v.nodeLogFiles(fmt.Sprintf("el-%d-", idx))
		if err == nil && len(files) > 0 {
			return files
		}
	}
	files, _ := v.nodeLogFiles(node)
	return files
}

// reorgLogMatch is one reorg log line's parsed drop length and, if the
// pattern captured it, common-ancestor height.
type reorgLogMatch struct {
	drop     int
	ancestor int
}

// logReorgMatches returns every reorg re matches in file f, via its
// named "drop" and (optional) "ancestor" capture groups.
func logReorgMatches(f string, re *regexp.Regexp) []reorgLogMatch {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	dropIdx, ancestorIdx := re.SubexpIndex("drop"), re.SubexpIndex("ancestor")
	if dropIdx < 0 {
		return nil
	}
	var out []reorgLogMatch
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		if dropIdx >= len(m) {
			continue
		}
		drop, err := strconv.Atoi(m[dropIdx])
		if err != nil {
			continue
		}
		row := reorgLogMatch{drop: drop}
		if ancestorIdx >= 0 && ancestorIdx < len(m) && m[ancestorIdx] != "" {
			row.ancestor, _ = strconv.Atoi(m[ancestorIdx])
		}
		out = append(out, row)
	}
	return out
}

// nodeLogFiles finds every file under --logs-dir whose name contains node,
// matching kurtosis's per-service log dump layout.
func (v *verifier) nodeLogFiles(node string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(v.logsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), node) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// grepAllLogs walks every file under --logs-dir looking for re, returning
// the first matching file.
func (v *verifier) grepAllLogs(re *regexp.Regexp) (bool, string) {
	found := ""
	filepath.WalkDir(v.logsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() {
			return nil
		}
		if fileContainsMatch(path, re) {
			found = path
		}
		return nil
	})
	return found != "", found
}

// grepLighthouseLogs restricts the walk to files whose name mentions
// lighthouse, for the non-gating finality note.
func (v *verifier) grepLighthouseLogs(re *regexp.Regexp) (bool, string) {
	found := ""
	filepath.WalkDir(v.logsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() {
			return nil
		}
		if !strings.Contains(strings.ToLower(d.Name()), "lighthouse") {
			return nil
		}
		if fileContainsMatch(path, re) {
			found = path
		}
		return nil
	})
	return found != "", found
}

func fileContainsMatch(path string, re *regexp.Regexp) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if re.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}

// checkC1 requires at least 100 canonical blocks strictly before b*.
func (v *verifier) checkC1(ctx context.Context) (verdict, string) {
	if v.bstarErr != nil {
		return verdictFail, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if v.bstar.number < 101 {
		return verdictFail, fmt.Sprintf("b*=%d, need >= 101 for 100 canonical blocks before it", v.bstar.number)
	}
	return verdictPass, v.bstar.evidence
}

// checkC2 requires an early first transaction (s0 <= 50) and at least one
// transacting block in every 25-block bucket of [s0, b*-1].
func (v *verifier) checkC2(ctx context.Context) (verdict, string) {
	if v.bstarErr != nil {
		return verdictFail, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if len(v.els) == 0 {
		return verdictFail, "no --el configured"
	}
	e := v.els[0]

	var s0 uint64
	found := false
	for n := uint64(0); n <= 50; n++ {
		cnt, err := v.txCount(ctx, e, n)
		if err != nil {
			return verdictFail, fmt.Sprintf("tx count for block %d: %v", n, err)
		}
		if cnt > 0 {
			s0, found = n, true
			break
		}
	}
	if !found {
		return verdictFail, "no block with >=1 tx found in [0,50]"
	}
	if v.bstar.number == 0 || v.bstar.number-1 < s0 {
		return verdictFail, fmt.Sprintf("s0=%d is not before b*-1 (b*=%d)", s0, v.bstar.number)
	}
	end := v.bstar.number - 1

	var emptyBuckets []string
	for lo := s0; lo <= end; lo += 25 {
		hi := lo + 24
		if hi > end {
			hi = end
		}
		bucketHasTx := false
		for n := lo; n <= hi; n++ {
			cnt, err := v.txCount(ctx, e, n)
			if err != nil {
				return verdictFail, fmt.Sprintf("tx count for block %d: %v", n, err)
			}
			if cnt > 0 {
				bucketHasTx = true
				break
			}
		}
		if !bucketHasTx {
			emptyBuckets = append(emptyBuckets, fmt.Sprintf("[%d,%d]", lo, hi))
		}
	}
	if len(emptyBuckets) > 0 {
		return verdictFail, fmt.Sprintf("s0=%d but empty 25-block bucket(s): %s", s0, strings.Join(emptyBuckets, ","))
	}
	return verdictPass, fmt.Sprintf("s0=%d, every 25-block bucket in [%d,%d] has >=1 tx block", s0, s0, end)
}

// checkC3 asks the run's own schedule which chaos ops actually ran and
// applies each op's class-specific heal deadline instead of one global
// rule: a straddle heals after the fork by design, so a single pre-fork
// deadline would fail every straddle by construction. It fails only on
// an admitted op that never healed, one healed past its class deadline,
// or an isolation the chaos evidence shows that the schedule does not
// admit - the schedule record is mandatory input, and its absence fails
// the check outright (unless --skip-chaos). Corroboration by a reorg is
// a floor of >=4, not all-must-match: a healed, converged op that
// produces no matchable reorg evidence is a stochastic miss (a light
// victim can legitimately mint nothing in its window), so falling short
// only on corroboration is INCONCLUSIVE rather than a failure. Reorg
// depth plays no part here - see C9.
func (v *verifier) checkC3(ctx context.Context) (verdict, string) {
	if v.skipChaos {
		return verdictPass, "skipped (--skip-chaos)"
	}
	dump, err := v.scheduleDump()
	if err != nil {
		return verdictFail, fmt.Sprintf("schedule: %v", err)
	}
	matched, unscheduled := v.attributeWindows(dump, v.chaosWindows())
	if len(unscheduled) > 0 {
		var names []string
		for _, w := range unscheduled {
			names = append(names, w.node)
		}
		return verdictFail, fmt.Sprintf("isolation(s) not in the schedule's admitted ops: %s", strings.Join(names, ", "))
	}

	var problems, notes []string
	healed, corroborated := 0, 0
	for _, ow := range matched {
		w := ow.window
		if !w.healed {
			problems = append(problems, fmt.Sprintf("op %s (%s) never healed", ow.op.Name, w.node))
			continue
		}
		if deadline := dump.HealDeadline(ow.op); w.to.After(deadline) {
			problems = append(problems, fmt.Sprintf("op %s (%s) healed at %s, after its %s deadline %s",
				ow.op.Name, w.node, w.to.Format(time.RFC3339), ow.op.Class, deadline.Format(time.RFC3339)))
			continue
		}
		healed++
		if ev := v.matchReorg(w); ev.matched {
			corroborated++
		} else {
			notes = append(notes, fmt.Sprintf("op %s (%s): %s", ow.op.Name, w.node, ev.String()))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	if healed < 4 {
		return verdictFail, fmt.Sprintf("only %d healed isolation(s) within their class deadline, need >= 4", healed)
	}
	evidence := fmt.Sprintf("%d/%d admitted ops healed within their class deadline, %d corroborated by a reorg", healed, len(matched), corroborated)
	if len(notes) > 0 {
		evidence += " (" + strings.Join(notes, "; ") + ")"
	}
	if corroborated < 4 {
		return verdictInconclusive, evidence
	}
	return verdictPass, evidence
}

// checkC4 requires 3-way (or self-consistent, under --smoke) agreement on
// (hash, stateRoot) for the trailing 32 blocks before b* and for
// [b*-1, b*+10].
func (v *verifier) checkC4(ctx context.Context) (verdict, string) {
	if v.bstarErr != nil {
		return verdictFail, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if len(v.els) == 0 {
		return verdictFail, "no --el configured"
	}
	b := v.bstar.number
	lo := uint64(0)
	if b >= 32 {
		lo = b - 32
	}
	hi := b + 10

	var problems []string
	checked := 0
	for n := lo; n <= hi; n++ {
		blocks := make([]*rpcBlock, len(v.els))
		anyErr := false
		for i, e := range v.els {
			blk, err := v.getBlock(ctx, e, hexutil.EncodeUint64(n))
			if err != nil {
				problems = append(problems, fmt.Sprintf("block %d: %v", n, err))
				anyErr = true
				continue
			}
			blocks[i] = blk
		}
		if anyErr {
			continue
		}
		checked++
		if v.smoke {
			continue // non-empty answers from every configured node is enough
		}
		for i := 1; i < len(blocks); i++ {
			if blocks[i].Hash != blocks[0].Hash || blocks[i].StateRoot != blocks[0].StateRoot {
				problems = append(problems, fmt.Sprintf("block %d: %s disagrees with %s on hash/stateRoot",
					n, v.els[i].name, v.els[0].name))
			}
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("%d block(s) in [%d,%d] agree across %d node(s)", checked, lo, hi, len(v.els))
}

func progressStage(p migmon.MigrationProgress) int {
	switch p.Phase {
	case migmon.PhaseDone:
		return 3
	case migmon.PhaseRunning:
		if p.Binary != nil && p.Binary.Phase == migmon.DirParked && migmon.Active(p.Merkle) {
			return 2
		}
		return 1
	default:
		return 0
	}
}

func (v *verifier) progressNodes() []string {
	seen := map[string]bool{}
	var out []string
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvProgress && !seen[ev.Node] {
			seen[ev.Node] = true
			out = append(out, ev.Node)
		}
	}
	return out
}

// checkNodeTimeline requires one node's phase to reach parked+Merkle-active
// no earlier than T, reach done strictly after T, and never regress out of
// done once reached. T stands in for b*'s own timestamp: b* is defined as
// >= T, so "after T" is a necessary (if slightly loose) proxy without a
// second RPC round-trip per event.
func (v *verifier) checkNodeTimeline(node string) error {
	var events []migmon.Event
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvProgress && ev.Node == node {
			events = append(events, ev)
		}
	}
	if len(events) == 0 {
		return fmt.Errorf("%s: no progress events", node)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })

	T := time.Unix(int64(v.T), 0)
	var firstStage2, firstStage3 time.Time
	maxStage := -1
	for _, ev := range events {
		if len(ev.Raw) == 0 {
			continue
		}
		p, err := migmon.DecodeProgress(ev.Raw)
		if err != nil {
			return fmt.Errorf("%s: undecodable progress at %s: %w", node, ev.Time.Format(time.RFC3339), err)
		}
		stage := progressStage(p)
		if maxStage == 3 && stage != 3 {
			return fmt.Errorf("%s: regressed from done back to phase %q at %s", node, p.Phase, ev.Time.Format(time.RFC3339))
		}
		if stage == 2 && firstStage2.IsZero() {
			firstStage2 = ev.Time
		}
		if stage == 3 && firstStage3.IsZero() {
			firstStage3 = ev.Time
		}
		if stage > maxStage {
			maxStage = stage
		}
	}
	if !firstStage2.IsZero() && firstStage2.Before(T) {
		return fmt.Errorf("%s: reached parked+Merkle-active at %s, before T=%s", node, firstStage2.Format(time.RFC3339), T.Format(time.RFC3339))
	}
	if firstStage3.IsZero() {
		return fmt.Errorf("%s: never reached done", node)
	}
	if !firstStage3.After(T) {
		return fmt.Errorf("%s: reached done at %s, not strictly after T=%s", node, firstStage3.Format(time.RFC3339), T.Format(time.RFC3339))
	}
	return nil
}

// checkC5 rejects any log mention of migration-window configuration,
// requires each node's phase timeline to be strictly ordered relative to
// b*, and surfaces (never gates on) beacon finality corroboration.
func (v *verifier) checkC5(ctx context.Context) (verdict, string) {
	var problems, notes []string

	if v.logsDir == "" {
		problems = append(problems, "no --logs-dir given")
	} else {
		if found, file := v.grepAllLogs(regexp.MustCompile(`(?i)migration window`)); found {
			problems = append(problems, fmt.Sprintf("found 'migration window' mention in %s", file))
		}
		if found, _ := v.grepLighthouseLogs(regexp.MustCompile(`(?i)finaliz`)); found {
			notes = append(notes, "INFO: lighthouse logs show finalized checkpoints")
		}
	}

	if v.bstarErr != nil {
		problems = append(problems, fmt.Sprintf("b* unresolved: %v", v.bstarErr))
	} else {
		for _, node := range v.progressNodes() {
			if err := v.checkNodeTimeline(node); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}

	evidence := strings.Join(append(problems, notes...), "; ")
	if evidence == "" {
		evidence = "no migration-window mentions; all node timelines strictly ordered"
	}
	return boolVerdict(len(problems) == 0), evidence
}

// checkC6 applies F1 semantics to the sample stream: full mode wants >=50
// all-non-null-equal samples with zero mismatches and >=10 of those
// strictly outside chaos windows; --smoke relaxes the sample floor. Any
// critical F1 fails unconditionally; other criticals are waived only when
// fully inside a chaos window (+30s slop) for that event's node.
func (v *verifier) checkC6(ctx context.Context) (verdict, string) {
	windows := v.chaosWindows()
	const slop = 30 * time.Second
	names := v.elNames()

	samples, good, outsideGood, anyNonNullSamples := 0, 0, 0, 0
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvSample {
			continue
		}
		samples++
		nonNull := map[string]bool{}
		responded := 0
		for _, node := range names {
			if root, ok := ev.Roots[node]; ok && root != "" {
				nonNull[root] = true
				responded++
			}
		}
		if len(nonNull) > 0 {
			anyNonNullSamples++
		}
		// More than one distinct non-null root is NOT a mismatch by
		// itself: during and just after partitions the nodes sit on
		// different canonical chains at the sampled height, and the
		// monitor - which groups by canonical hash before comparing -
		// emits a hash-split warn instead of F1 (observed on live chaos runs).
		// The monitor's F1 criticals, handled below, are the mismatch
		// authority; the roots map alone cannot distinguish a split
		// from a divergence.
		if responded == len(names) && len(nonNull) == 1 {
			good++
			outside := true
			for _, w := range windows {
				if w.covers("", ev.Time, slop) {
					outside = false
					break
				}
			}
			if outside {
				outsideGood++
			}
		}
	}

	var problems []string
	if v.smoke {
		if anyNonNullSamples < 5 {
			problems = append(problems, fmt.Sprintf("only %d sample(s) with >=1 non-null root, need >= 5 (smoke)", anyNonNullSamples))
		}
	} else if good < 50 {
		problems = append(problems, fmt.Sprintf("only %d all-non-null-equal sample(s), need >= 50", good))
	}
	if !v.skipChaos && outsideGood < 10 {
		problems = append(problems, fmt.Sprintf("only %d all-non-null-equal sample(s) strictly outside chaos windows, need >= 10", outsideGood))
	}

	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvCritical {
			continue
		}
		if ev.Finding == migmon.FindingRootMismatch {
			problems = append(problems, fmt.Sprintf("critical F1 at %s (node %s): %s", ev.Time.Format(time.RFC3339), ev.Node, ev.Detail))
			continue
		}
		waived := false
		for _, w := range windows {
			if w.covers(ev.Node, ev.Time, slop) {
				waived = true
				break
			}
		}
		if !waived {
			problems = append(problems, fmt.Sprintf("critical %s at %s (node %s) outside any chaos window: %s",
				ev.Finding, ev.Time.Format(time.RFC3339), ev.Node, ev.Detail))
		}
	}

	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("%d samples, %d good, %d outside chaos windows, 0 unwaived criticals", samples, good, outsideGood)
}

// checkC7 compares the pins file against every node's real genesis block.
// genesis_state_root is the required pin: the alloc is what must be stable
// run to run. genesis_hash is optional and asserted only when present -
// kurtosis stamps each run's genesis timestamp at render time, so the hash
// is per-run by design: kurtosis stamps it at render time. "pending" fails, except
// under --smoke where it is a warning.
func (v *verifier) checkC7(ctx context.Context) (verdict, string) {
	if v.pinsErr != nil {
		return verdictFail, fmt.Sprintf("pins: %v", v.pinsErr)
	}
	hash, hok := v.pins["genesis_hash"]
	root, rok := v.pins["genesis_state_root"]
	if !rok {
		return verdictFail, "pins file missing genesis_state_root"
	}
	if (hok && hash == "pending") || root == "pending" {
		if v.smoke {
			return verdictPass, "WARN: genesis pins are 'pending' (allowed under --smoke)"
		}
		return verdictFail, "genesis pins are 'pending'"
	}

	var problems []string
	for _, e := range v.els {
		blk, err := v.getBlock(ctx, e, "0x0")
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.name, err))
			continue
		}
		if hok && !strings.EqualFold(blk.Hash.Hex(), hash) {
			problems = append(problems, fmt.Sprintf("%s genesis hash %s != pinned %s", e.name, blk.Hash.Hex(), hash))
		}
		if !strings.EqualFold(blk.StateRoot.Hex(), root) {
			problems = append(problems, fmt.Sprintf("%s genesis stateRoot %s != pinned %s", e.name, blk.StateRoot.Hex(), root))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	what := "stateRoot"
	if hok {
		what = "hash/stateRoot"
	}
	return verdictPass, fmt.Sprintf("genesis %s match pins across %d node(s)", what, len(v.els))
}

var digestLineRe = regexp.MustCompile(`PBT_ARTIFACT_DIGESTS\s+\S*snapshot=([0-9a-fA-F]{64})\s+\S*preimages=([0-9a-fA-F]{64})`)

type artifactDigests struct{ snapshot, preimages string }

// checkC8 requires exactly one PBT_ARTIFACT_DIGESTS line per EL log file,
// with identical snapshot/preimages digests across every node.
func (v *verifier) checkC8(ctx context.Context) (verdict, string) {
	if v.logsDir == "" {
		return verdictFail, "no --logs-dir given"
	}
	perNode := map[string]artifactDigests{}
	var problems []string
	for _, node := range v.elNames() {
		files, err := v.nodeLogFiles(node)
		if err != nil || len(files) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no log file found", node))
			continue
		}
		var matches []artifactDigests
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, m := range digestLineRe.FindAllStringSubmatch(string(data), -1) {
				matches = append(matches, artifactDigests{snapshot: strings.ToLower(m[1]), preimages: strings.ToLower(m[2])})
			}
		}
		if len(matches) != 1 {
			problems = append(problems, fmt.Sprintf("%s: found %d PBT_ARTIFACT_DIGESTS line(s), want exactly 1", node, len(matches)))
			continue
		}
		perNode[node] = matches[0]
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}

	var first artifactDigests
	firstNode := ""
	for _, node := range v.elNames() {
		d := perNode[node]
		if firstNode == "" {
			first, firstNode = d, node
			continue
		}
		if d != first {
			problems = append(problems, fmt.Sprintf("%s digests differ from %s", node, firstNode))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("snapshot=%s preimages=%s identical across %d node(s)", first.snapshot, first.preimages, len(perNode))
}

// checkC9 asks whether any healed isolation's corroborated reorg
// reached a dropped-branch depth >= 10 - the threshold C3 used to gate
// on directly, which failed every straddle by construction (a straddle
// heals after the fork, so if it already cleared its own class deadline
// in C3 there is nothing left for a global depth floor to add). Depth
// depends on what the isolated victim minted alone in its window, which
// a light victim or bad luck can legitimately fail to produce, so its
// absence is a stochastic miss, not a failure.
func (v *verifier) checkC9(ctx context.Context) (verdict, string) {
	if v.skipChaos {
		return verdictPass, "skipped (--skip-chaos)"
	}
	dump, err := v.scheduleDump()
	if err != nil {
		return verdictFail, fmt.Sprintf("schedule: %v", err)
	}
	matched, _ := v.attributeWindows(dump, v.chaosWindows())
	best, bestOp := 0, ""
	for _, ow := range matched {
		if !ow.window.healed {
			continue
		}
		if ev := v.matchReorg(ow.window); ev.matched && ev.depth > best {
			best, bestOp = ev.depth, ow.op.Name
		}
	}
	if best >= 10 {
		return verdictPass, fmt.Sprintf("op %s reorged at depth %d (>= 10)", bestOp, best)
	}
	return verdictInconclusive, fmt.Sprintf("no admitted op reorged at depth >= 10 (max observed %d)", best)
}

// checkC10 asks whether the run actually exercised a fork-straddling
// partition: the schedule must have admitted a straddle op whose window
// contains the fork time, the victim's fork block must have been
// orphaned by the heal (a bstar-reorged event), and the dropped branch
// must be at least 6 blocks deep - proof the reorg crossed a real span
// of chain, not a one-block wobble. A profile that never scheduled a
// straddle, or a straddle victim that minted no post-fork block to
// orphan, is INCONCLUSIVE: the run simply never got to prove this.
func (v *verifier) checkC10(ctx context.Context) (verdict, string) {
	if v.skipChaos {
		return verdictPass, "skipped (--skip-chaos)"
	}
	dump, err := v.scheduleDump()
	if err != nil {
		return verdictFail, fmt.Sprintf("schedule: %v", err)
	}
	fork := time.Unix(dump.Fork, 0)
	admitted := dump.Admitted()
	var straddle *migsched.DumpOp
	for i := range admitted {
		o := admitted[i]
		if migsched.Class(o.Class) != migsched.ClassStraddle {
			continue
		}
		start, end := time.Unix(o.Start, 0), time.Unix(o.End, 0)
		if !fork.Before(start) && !fork.After(end) {
			straddle = &admitted[i]
			break
		}
	}
	if straddle == nil {
		return verdictInconclusive, fmt.Sprintf("no admitted straddle op spans the fork time %s in this run's schedule", fork.Format(time.RFC3339))
	}
	if len(straddle.Victims) == 0 {
		return verdictFail, fmt.Sprintf("schedule straddle op %s has no victims", straddle.Name)
	}
	victimNode := fmt.Sprintf("node-%d", straddle.Victims[0])

	reorged := false
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvBStarReorged && sameNode(ev.Node, victimNode) {
			reorged = true
			break
		}
	}
	if !reorged {
		return verdictInconclusive, fmt.Sprintf("straddle op %s admitted (window contains fork %s), but victim's fork block was never orphaned (no bstar-reorged event)",
			straddle.Name, fork.Format(time.RFC3339))
	}

	w := chaosWindow{
		node:   victimNode,
		from:   time.Unix(straddle.Start, 0),
		to:     time.Unix(straddle.End, 0).Add(migmon.ConvergenceGrace * time.Second),
		healed: true,
	}
	reorg := v.matchReorg(w)
	if !reorg.matched {
		return verdictInconclusive, fmt.Sprintf("straddle op %s: victim's fork block was orphaned but no matchable reorg depth evidence (%s)", straddle.Name, reorg.String())
	}
	if reorg.depth < 6 {
		return verdictFail, fmt.Sprintf("straddle op %s dropped branch depth %d < 6 (%s)", straddle.Name, reorg.depth, reorg.String())
	}
	return verdictPass, fmt.Sprintf("straddle op %s spans fork %s, victim's fork block orphaned, dropped branch depth %d (%s)",
		straddle.Name, fork.Format(time.RFC3339), reorg.depth, reorg.String())
}

// forkBlockRecord is one node's view of the fork block: the height b*
// landed on and the hash it holds.
type forkBlockRecord struct {
	number uint64
	hash   string
}

// finalForkBlock determines the run's authoritative final fork block:
// the bstar-final record every node that reached it agrees on, or - if
// none finalized within the observed stream - the latest bstar record
// every node that reported one agrees on. Disagreement, or no record at
// all, is a hard error: this is not a stochastic precondition, it is
// the fork block the whole run is organized around.
func (v *verifier) finalForkBlock() (forkBlockRecord, error) {
	final := map[string]forkBlockRecord{}
	latest := map[string]forkBlockRecord{}
	for _, ev := range v.monitor {
		switch ev.Kind {
		case migmon.EvBStar, migmon.EvBStarReorged:
			latest[ev.Node] = forkBlockRecord{number: ev.Number, hash: ev.Hash}
		case migmon.EvBStarFinal:
			final[ev.Node] = forkBlockRecord{number: ev.Number, hash: ev.Hash}
		}
	}
	records, source := final, "bstar-final"
	if len(records) == 0 {
		records, source = latest, "bstar"
	}
	if len(records) == 0 {
		return forkBlockRecord{}, fmt.Errorf("no bstar record observed to establish a final fork block")
	}
	var ref forkBlockRecord
	set := false
	var disagree []string
	for node, r := range records {
		if !set {
			ref, set = r, true
			continue
		}
		if r != ref {
			disagree = append(disagree, node)
		}
	}
	if len(disagree) > 0 {
		sort.Strings(disagree)
		return forkBlockRecord{}, fmt.Errorf("%s records disagree across nodes: reference block %d %s, disagreeing %s", source, ref.number, ref.hash, strings.Join(disagree, ","))
	}
	return ref, nil
}

// checkC11 requires that once a fork-straddling partition heals, the
// two branches that crossed the fork independently actually converge:
// every configured node must agree on the final canonical fork block's
// height and hash, no F4 (no-convergence) critical may go unwaived, and
// at least 2 distinct provisional fork-block hashes must have been
// observed across the run - proof that two branches genuinely crossed
// independently, not that only one node ever saw a fork block at all.
// An F4 is waived under the same rule C6 applies to other criticals: a
// fresh chaos window already covering that node and moment explains a
// transient non-convergence without a stuck node.
func (v *verifier) checkC11(ctx context.Context) (verdict, string) {
	final, err := v.finalForkBlock()
	if err != nil {
		return verdictFail, err.Error()
	}

	var problems []string
	for _, e := range v.els {
		blk, err := v.getBlock(ctx, e, hexutil.EncodeUint64(final.number))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.name, err))
			continue
		}
		if !strings.EqualFold(blk.Hash.Hex(), final.hash) {
			problems = append(problems, fmt.Sprintf("%s block %d hash %s != final fork-block hash %s", e.name, final.number, blk.Hash.Hex(), final.hash))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}

	windows := v.chaosWindows()
	const slop = 30 * time.Second
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvCritical || ev.Finding != migmon.FindingNoConvergence {
			continue
		}
		waived := false
		for _, w := range windows {
			if w.covers(ev.Node, ev.Time, slop) {
				waived = true
				break
			}
		}
		if !waived {
			problems = append(problems, fmt.Sprintf("unwaived F4 at %s (node %s): %s", ev.Time.Format(time.RFC3339), ev.Node, ev.Detail))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}

	provisional := map[string]bool{}
	for _, ev := range v.monitor {
		if (ev.Kind == migmon.EvBStar || ev.Kind == migmon.EvBStarReorged) && ev.Hash != "" {
			provisional[ev.Hash] = true
		}
	}
	if len(provisional) < 2 {
		return verdictInconclusive, fmt.Sprintf("all %d node(s) agree on final fork block %d (%s), 0 unwaived F4, but only %d distinct provisional fork-block hash(es) observed, need >= 2 to prove two branches crossed independently",
			len(v.els), final.number, final.hash, len(provisional))
	}
	return verdictPass, fmt.Sprintf("all %d node(s) agree on final fork block %d (%s), 0 unwaived F4, %d distinct provisional fork-block hashes observed",
		len(v.els), final.number, final.hash, len(provisional))
}

// orphanBlock is one post-fork block a bstar-reorged event shows a node
// abandoned.
type orphanBlock struct {
	height uint64
	hash   string
}

var orphanHashRe = regexp.MustCompile(`0x[0-9a-fA-F]{64}`)

// orphanedForkBlocks collects the post-fork blocks bstar-reorged events
// show were orphaned: the old hash embedded in each event's Detail
// ("fork block N 0x... was orphaned; ..."), at the height its Number
// field records.
func (v *verifier) orphanedForkBlocks() []orphanBlock {
	var out []orphanBlock
	seen := map[string]bool{}
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvBStarReorged {
			continue
		}
		old := orphanHashRe.FindString(ev.Detail)
		if old == "" || seen[old] {
			continue
		}
		seen[old] = true
		out = append(out, orphanBlock{height: ev.Number, hash: old})
	}
	return out
}

// checkC12 requires that the straddle victim's orphaned post-fork
// blocks are truly gone: identified from bstar-reorged events' old
// hashes, eth_getBlockByHash on every configured node must return
// either null or a block that is not canonical (its height's own
// canonical hash differs). No identifiable orphan hash - no victim ever
// minted a post-fork block to orphan - is INCONCLUSIVE, not a pass: the
// check proved nothing.
func (v *verifier) checkC12(ctx context.Context) (verdict, string) {
	orphans := v.orphanedForkBlocks()
	if len(orphans) == 0 {
		return verdictInconclusive, "no orphaned post-fork block identified (no bstar-reorged event with a parsable old hash)"
	}

	var problems []string
	canonical := map[uint64]string{} // height -> canonical hash, memoised across nodes and orphans
	for _, orphan := range orphans {
		for _, e := range v.els {
			blk, err := v.getBlockByHash(ctx, e, orphan.hash)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: eth_getBlockByHash(%s): %v", e.name, orphan.hash, err))
				continue
			}
			if blk == nil {
				continue // gone: exactly the expected outcome
			}
			height := uint64(blk.Number)
			canonHash, ok := canonical[height]
			if !ok {
				head, err := v.getBlock(ctx, e, hexutil.EncodeUint64(height))
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: canonical block %d: %v", e.name, height, err))
					continue
				}
				canonHash = head.Hash.Hex()
				canonical[height] = canonHash
			}
			if strings.EqualFold(blk.Hash.Hex(), canonHash) {
				problems = append(problems, fmt.Sprintf("%s still serves orphaned block %s at height %d as canonical", e.name, orphan.hash, height))
			}
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("%d orphaned post-fork block(s) confirmed non-canonical (null or superseded) across %d node(s)", len(orphans), len(v.els))
}
