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

// verifier holds every input the checks read plus the resolved I*; checks run in sequence.
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

	// manifest is the lap driver's own record of its actions; nil when driven by hand.
	manifestPath string
	manifest     *lapManifest
	manifestErr  error

	istar    istarResult
	istarErr error

	// schedule caches the chaos stream's "schedule" record.
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

// elByIndex resolves a participant index to its EL via the el-N-... naming convention.
func (v *verifier) elByIndex(idx int) (el, bool) {
	for _, e := range v.els {
		if i, ok := nodeIndex(e.name); ok && i == idx {
			return e, true
		}
	}
	return el{}, false
}

// legacyFindings maps pre-rename finding codes to their current names.
var legacyFindings = map[string]string{
	"F1": migmon.FindingRootMismatch, "F2": migmon.FindingStall, "F3": migmon.FindingBoundary,
	"F4": migmon.FindingNoConvergence, "NULL5": migmon.FindingNullWarn, "NULL10": migmon.FindingNullCritical,
}

// readEvents parses one JSONL stream of migmon.Event lines, sorted by time;
// non-event lines are skipped and legacy kinds are normalized.
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
		// Other processes write structured lines into the same stdout; skip non-events.
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
		ev.Kind = strings.Replace(ev.Kind, "bstar", "istar", 1)
		if name, ok := legacyFindings[ev.Finding]; ok {
			ev.Finding = name
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

// parsePins reads "key: value" lines; blank lines and "#" comments are ignored.
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

// istarResult is the resolved I*: the first canonical block whose header time is >= T.
type istarResult struct {
	number   uint64
	evidence string
}

// resolveIStar cross-checks the monitor's istar events against an RPC backwalk on
// els[0]; any disagreement fails every check that needs I*.
func (v *verifier) resolveIStar(ctx context.Context) {
	nodeVals := map[string]uint64{}
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvIStar {
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
		v.istarErr = fmt.Errorf("monitor istar events disagree across nodes: %v", nodeVals)
		return
	}

	var rpcNum uint64
	var rpcErr error
	if len(v.els) == 0 {
		rpcErr = fmt.Errorf("no --el configured")
	} else {
		rpcNum, rpcErr = v.backwalkIStar(ctx, v.els[0])
	}

	switch {
	case monitorSet && rpcErr == nil && monitorNum == rpcNum:
		v.istar = istarResult{monitorNum, fmt.Sprintf("I*=%d (monitor istar events agree with rpc backwalk)", monitorNum)}
	case monitorSet && rpcErr == nil:
		v.istarErr = fmt.Errorf("monitor istar=%d disagrees with rpc backwalk=%d", monitorNum, rpcNum)
	case monitorSet:
		v.istar = istarResult{monitorNum, fmt.Sprintf("I*=%d (monitor istar events only, rpc backwalk failed: %v)", monitorNum, rpcErr)}
	case rpcErr == nil:
		v.istar = istarResult{rpcNum, fmt.Sprintf("I*=%d (rpc backwalk only, no monitor istar events)", rpcNum)}
	default:
		v.istarErr = fmt.Errorf("no monitor istar events and rpc backwalk failed: %w", rpcErr)
	}
}

// backwalkIStar binary-searches els[0] for the first block whose header timestamp is >= T.
func (v *verifier) backwalkIStar(ctx context.Context, e el) (uint64, error) {
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

// chaosWindow is one isolate-to-heal span for a victim node ("" = heal-all).
// An unhealed isolation has a zero `to` and healed=false.
type chaosWindow struct {
	node   string
	from   time.Time
	to     time.Time
	healed bool
}

// covers reports whether t (± slop) falls inside the window for node; an empty
// node on either side matches any node.
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

// chaosWindows pairs each isolate with the same-node heal or heal-all that closes it.
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

// scheduleDump returns the schedule from the chaos stream's first "schedule"
// event (Raw is a migsched.Dump); its absence is an error.
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

// opAttributionSlop tolerates the gap between an op's [Start, End] and its actual isolate/heal times.
const opAttributionSlop = 30 * time.Second

// opWindow pairs one admitted schedule op with the chaosWindow recorded for it.
type opWindow struct {
	op     migsched.DumpOp
	window chaosWindow
}

// waiverWindows is chaosWindows with each straddle-attributed window's close
// extended by migmon.SplitGrace, the bound the monitor's split criticals fire against.
func (v *verifier) waiverWindows() []chaosWindow {
	windows := v.chaosWindows()
	dump, err := v.scheduleDump()
	if err != nil {
		return windows
	}
	for i, w := range windows {
		if w.to.IsZero() {
			continue
		}
		for _, op := range dump.Admitted() {
			if migsched.Class(op.Class) == migsched.ClassStraddle && opOwns(op, w) {
				windows[i].to = w.to.Add(migmon.SplitGrace)
				break
			}
		}
	}
	return windows
}

// attributeWindows matches each recorded chaos window to the admitted op owning
// it; unmatched windows are unscheduled chaos.
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

// opOwns: w's victim index is one of op's and w started inside op's [Start, End] ± opAttributionSlop.
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

// clientLogPatterns maps a client name (from "el-<n>-<client>-<cl>") to its reorg
// log regexp with named captures "drop" and optional "ancestor". Unregistered
// clients degrade to monitor-events-only corroboration.
var clientLogPatterns = map[string]*regexp.Regexp{
	// geth: number= is the common-ancestor height, drop= the dropped-branch length.
	"geth": regexp.MustCompile(`Chain reorg detected.*\bnumber=(?P<ancestor>\d+).*\bdrop=(?P<drop>\d+)`),
}

// clientOf extracts the client name from an "el-<n>-<client>-<cl>" name; "node-<n>" yields "".
func clientOf(node string) string {
	parts := strings.Split(node, "-")
	if len(parts) < 4 || parts[0] != "el" {
		return ""
	}
	return parts[2]
}

var nodeIndexRe = regexp.MustCompile(`^(?:node|el)-(\d+)\b|^(?:node|el)-(\d+)-`)

// nodeIndex extracts the participant index from "node-N" or "el-N-..." names.
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

// sameNode reports whether two names denote the same participant index.
func sameNode(a, b string) bool {
	if a == b {
		return true
	}
	ia, oka := nodeIndex(a)
	ib, okb := nodeIndex(b)
	return oka && okb && ia == ib
}

// victimClient resolves node's client type via clientOf, then via the monitor's and logsDir's el-N-<client> names.
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

// reorgEvidence is what the run can prove about the reorg tied to one isolation's heal.
type reorgEvidence struct {
	depth    int
	matched  bool
	ancestor int    // common-ancestor block height, 0 if unknown
	source   string // "monitor", or a client name for a log-line match
	// degraded: the victim's client has no registered log pattern.
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

// matchReorg finds reorg evidence for w. The client's own reorg log line is
// the authority: its drop is the dropped branch length. Monitor reorg events
// only prove a reorg happened; their number is how far behind the head a
// re-checked height sat, not a branch length, so they are the fallback.
func (v *verifier) matchReorg(w chaosWindow) reorgEvidence {
	const slop = 30 * time.Second
	var best reorgEvidence
	client := v.victimClient(w.node)
	re, registered := clientLogPatterns[client]
	if registered && v.logsDir != "" {
		for _, f := range v.victimLogFiles(w.node) {
			for _, m := range logReorgMatches(f, re, w.from) {
				// a log line is evidence for THIS window only if it was written inside it
				if m.at.IsZero() || !w.covers("", m.at, slop) {
					continue
				}
				if m.drop >= 1 && m.drop > best.depth {
					best = reorgEvidence{depth: m.drop, matched: true, source: client, ancestor: m.ancestor}
				}
			}
		}
	}
	if best.matched {
		return best
	}
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvReorg || !sameNode(ev.Node, w.node) || !w.covers(ev.Node, ev.Time, slop) {
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
	best.degraded = !registered && !best.matched
	return best
}

// victimLogFiles resolves a chaos victim name to its log files by participant index, then by substring.
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

// reorgLogMatch is one reorg log line: drop length, optional common-ancestor height, and when it was logged.
type reorgLogMatch struct {
	drop     int
	ancestor int
	at       time.Time
}

// geth stamps lines "[MM-DD|HH:MM:SS.mmm]" in UTC without a year; ref supplies it.
var logStampRe = regexp.MustCompile(`\[(\d\d)-(\d\d)\|(\d\d):(\d\d):(\d\d)\.\d+\]`)

// logReorgMatches returns every line of f matching re, with its "drop", optional "ancestor", and timestamp.
func logReorgMatches(f string, re *regexp.Regexp, ref time.Time) []reorgLogMatch {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	dropIdx, ancestorIdx := re.SubexpIndex("drop"), re.SubexpIndex("ancestor")
	if dropIdx < 0 {
		return nil
	}
	var out []reorgLogMatch
	for _, line := range strings.Split(string(data), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil || dropIdx >= len(m) {
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
		if s := logStampRe.FindStringSubmatch(line); s != nil {
			n := func(i int) int { v, _ := strconv.Atoi(s[i]); return v }
			row.at = time.Date(ref.UTC().Year(), time.Month(n(1)), n(2), n(3), n(4), n(5), 0, time.UTC)
		}
		out = append(out, row)
	}
	return out
}

// nodeLogFiles finds every file under --logs-dir whose name contains node.
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

// grepAllLogs returns the first file under --logs-dir matching re.
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

// grepLighthouseLogs restricts the walk to files whose name mentions lighthouse.
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

// checkChainBeforeFork asserts >= 100 canonical blocks before I*, scaled to 3/4 of
// the schedule's pre-fork slots on short profiles. Fails when I* is unresolved.
func (v *verifier) checkChainBeforeFork(ctx context.Context) (verdict, string) {
	if v.istarErr != nil {
		return verdictFail, fmt.Sprintf("I* unresolved: %v", v.istarErr)
	}
	min := uint64(101)
	if dump, err := v.scheduleDump(); err == nil && dump.SlotSeconds > 0 && dump.Fork > dump.Genesis {
		if derived := uint64((dump.Fork - dump.Genesis) / dump.SlotSeconds * 3 / 4); derived < min {
			min = derived
		}
	}
	if v.istar.number < min {
		return verdictFail, fmt.Sprintf("I*=%d, need >= %d canonical blocks before the fork for this profile", v.istar.number, min)
	}
	return verdictPass, v.istar.evidence
}

// checkTrafficCoverage asserts a first transaction at s0 <= 50 and >= 1 transacting
// block in every 25-block bucket of [s0, I*-1].
func (v *verifier) checkTrafficCoverage(ctx context.Context) (verdict, string) {
	if v.istarErr != nil {
		return verdictFail, fmt.Sprintf("I* unresolved: %v", v.istarErr)
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
	if v.istar.number == 0 || v.istar.number-1 < s0 {
		return verdictFail, fmt.Sprintf("s0=%d is not before I*-1 (I*=%d)", s0, v.istar.number)
	}
	end := v.istar.number - 1

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

// checkPartitionsHealed asserts every admitted op healed within its class deadline
// and no isolation falls outside the schedule. INCONCLUSIVE when < 4 ops healed
// or < 4 are corroborated by a reorg.
func (v *verifier) checkPartitionsHealed(ctx context.Context) (verdict, string) {
	if v.skipChaos {
		return verdictPass, "skipped (--skip-chaos)"
	}
	dump, err := v.scheduleDump()
	if err != nil {
		return verdictFail, fmt.Sprintf("schedule: %v", err)
	}
	matched, unscheduled := v.attributeWindows(dump, v.chaosWindows())
	// Isolations after the schedule's quiet instant are post-migration and judged
	// by the boundary and orphan checks; without a quiet instant, every unattributed one is stray.
	quiet := time.Unix(dump.Quiet, 0)
	var afterQuiet, stray []chaosWindow
	for _, w := range unscheduled {
		if dump.Quiet != 0 && w.from.After(quiet) {
			afterQuiet = append(afterQuiet, w)
		} else {
			stray = append(stray, w)
		}
	}
	if len(stray) > 0 {
		var names []string
		for _, w := range stray {
			names = append(names, w.node)
		}
		return verdictFail, fmt.Sprintf("isolation(s) not in the schedule's admitted ops: %s", strings.Join(names, ", "))
	}

	var problems, notes []string
	if len(afterQuiet) > 0 {
		notes = append(notes, fmt.Sprintf("%d post-migration isolation(s) applied after the schedule went quiet", len(afterQuiet)))
	}
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
	// The >= 4 floor is a full-run requirement; a shorter profile is thin, not broken.
	if healed < len(matched) {
		return verdictFail, fmt.Sprintf("only %d of %d admitted isolations healed within their class deadline", healed, len(matched))
	}
	if healed < 4 {
		return verdictInconclusive, fmt.Sprintf("all %d admitted isolation(s) healed within their deadline, but a full run schedules >= 4", healed)
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

// checkBoundaryAgreement asserts all nodes agree on (hash, stateRoot) for
// [I*-32, I*+10]; --smoke only requires a non-empty answer from every node.
func (v *verifier) checkBoundaryAgreement(ctx context.Context) (verdict, string) {
	if v.istarErr != nil {
		return verdictFail, fmt.Sprintf("I* unresolved: %v", v.istarErr)
	}
	if len(v.els) == 0 {
		return verdictFail, "no --el configured"
	}
	b := v.istar.number
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

// checkNoConfiguredWindow asserts no EL log mentions a configured migration
// window; lighthouse finality is reported as a non-gating note.
func (v *verifier) checkNoConfiguredWindow(ctx context.Context) (verdict, string) {
	var problems, notes []string

	if v.logsDir == "" {
		problems = append(problems, "no --logs-dir given")
	} else {
		// Only EL logs count; the forbidden pattern is per-client from the registry.
		for _, node := range v.elNames() {
			spec, known := migmon.SpecFor(node)
			if !known || spec.ForbiddenWindowLog == "" {
				notes = append(notes, fmt.Sprintf("%s: no registered window-log contract", node))
				continue
			}
			re := regexp.MustCompile(`(?i)` + spec.ForbiddenWindowLog)
			for _, f := range v.victimLogFiles(node) {
				if fileContainsMatch(f, re) {
					problems = append(problems, fmt.Sprintf("found %q mention in %s", spec.ForbiddenWindowLog, f))
					break
				}
			}
		}
		if found, _ := v.grepLighthouseLogs(regexp.MustCompile(`(?i)finaliz`)); found {
			notes = append(notes, "INFO: lighthouse logs show finalized checkpoints")
		}
	}

	evidence := strings.Join(append(problems, notes...), "; ")
	if evidence == "" {
		evidence = "no configured-window mention in any client log"
	}
	return boolVerdict(len(problems) == 0), evidence
}

// checkShadowSamples asserts zero root-mismatch criticals, other criticals only
// inside waiver windows (±30s), and >= 10 agreeing samples outside chaos.
// INCONCLUSIVE (full mode) when < 50 agreeing samples or < 4 in [I*-40, I*+10].
func (v *verifier) checkShadowSamples(ctx context.Context) (verdict, string) {
	windows := v.chaosWindows()
	const slop = 30 * time.Second
	names := v.elNames()

	samples, good, outsideGood, anyNonNullSamples := 0, 0, 0, 0
	// goodNearBoundary counts agreeing samples in the boundary band [I*-40, I*+10].
	goodNearBoundary := 0
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
		// Several distinct non-null roots is a split, not a mismatch; the monitor's
		// root-mismatch criticals are the authority.
		if responded == len(names) && len(nonNull) == 1 {
			good++
			if v.istarErr == nil && ev.Number+40 >= v.istar.number && ev.Number <= v.istar.number+10 {
				goodNearBoundary++
			}
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
	thin := ""
	if v.smoke {
		if anyNonNullSamples < 5 {
			problems = append(problems, fmt.Sprintf("only %d sample(s) with >=1 non-null root, need >= 5 (smoke)", anyNonNullSamples))
		}
	} else if good < 50 {
		thin = fmt.Sprintf("only %d all-non-null-equal sample(s), want >= 50 for a full run", good)
	}
	if !v.skipChaos && outsideGood < 10 {
		problems = append(problems, fmt.Sprintf("only %d all-non-null-equal sample(s) strictly outside chaos windows, need >= 10", outsideGood))
	}
	boundaryThin := ""
	if !v.smoke && v.istarErr == nil && goodNearBoundary < 4 {
		boundaryThin = fmt.Sprintf("only %d agreeing sample(s) in the boundary band [I*-40, I*+10], want >= 4", goodNearBoundary)
	}

	waivers := v.waiverWindows()
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvCritical {
			continue
		}
		if ev.Finding == migmon.FindingRootMismatch {
			problems = append(problems, fmt.Sprintf("critical root-mismatch at %s (node %s): %s", ev.Time.Format(time.RFC3339), ev.Node, ev.Detail))
			continue
		}
		waived := false
		for _, w := range waivers {
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
	if thin != "" || boundaryThin != "" {
		reasons := strings.TrimSuffix(strings.TrimPrefix(thin+"; "+boundaryThin, "; "), "; ")
		return verdictInconclusive, fmt.Sprintf("%s (%d samples, %d good, %d outside chaos windows, %d near boundary, 0 unwaived criticals)",
			reasons, samples, good, outsideGood, goodNearBoundary)
	}
	return verdictPass, fmt.Sprintf("%d samples, %d good, %d outside chaos windows, %d near boundary, 0 unwaived criticals", samples, good, outsideGood, goodNearBoundary)
}

// checkGenesisPins asserts every node's genesis matches genesis_state_root (required)
// and genesis_hash (optional). "pending" fails, except under --smoke.
func (v *verifier) checkGenesisPins(ctx context.Context) (verdict, string) {
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

// checkPreForkDeepReorg asserts some admitted op entirely before the fork reorged
// at depth >= 10. INCONCLUSIVE when none did.
func (v *verifier) checkPreForkDeepReorg(ctx context.Context) (verdict, string) {
	if v.skipChaos {
		return verdictPass, "skipped (--skip-chaos)"
	}
	dump, err := v.scheduleDump()
	if err != nil {
		return verdictFail, fmt.Sprintf("schedule: %v", err)
	}
	matched, _ := v.attributeWindows(dump, v.chaosWindows())
	fork := time.Unix(dump.Fork, 0)
	best, bestOp := 0, ""
	for _, ow := range matched {
		// Only ops entirely before the fork count; straddle-rewind judges the straddle's reorg.
		if !time.Unix(ow.op.End, 0).Before(fork) {
			continue
		}
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

// checkStraddleRewind asserts an admitted straddle op spans the fork, the victim's
// fork block was orphaned (istar-reorged), and the dropped branch is >= 6 deep.
// INCONCLUSIVE when no straddle spans the fork, no orphan event, or no depth evidence.
func (v *verifier) checkStraddleRewind(ctx context.Context) (verdict, string) {
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
		if ev.Kind == migmon.EvIStarReorged && sameNode(ev.Node, victimNode) {
			reorged = true
			break
		}
	}
	if !reorged {
		return verdictInconclusive, fmt.Sprintf("straddle op %s admitted (window contains fork %s), but victim's fork block was never orphaned (no istar-reorged event)",
			straddle.Name, fork.Format(time.RFC3339))
	}

	w := chaosWindow{
		node:   victimNode,
		from:   time.Unix(straddle.Start, 0),
		to:     time.Unix(straddle.End, 0).Add(migmon.SplitGrace),
		healed: true,
	}
	reorg := v.matchReorg(w)
	// The parent walk on the victim's own chain data is the primary depth source;
	// matchReorg is corroboration and fallback.
	if orphans := v.orphanedForkBlocks(); len(orphans) > 0 {
		if victim, ok := v.elByIndex(straddle.Victims[0]); ok {
			if spec, known := migmon.SpecFor(victim.name); known && spec.ServesOrphans {
				tip := v.victimTip(victimNode, w.from, w.to)
				if depth, ancestor, err := v.walkOrphanBranch(ctx, victim, orphans[0].hash, tip); err == nil {
					reorg = reorgEvidence{depth: depth, matched: true, ancestor: ancestor,
						source: fmt.Sprintf("parent walk (corroboration: %s)", reorg.String())}
				}
			}
		}
	}
	if !reorg.matched {
		return verdictInconclusive, fmt.Sprintf("straddle op %s: victim's fork block was orphaned but no matchable reorg depth evidence (%s)", straddle.Name, reorg.String())
	}
	if reorg.depth < 6 {
		return verdictFail, fmt.Sprintf("straddle op %s dropped branch depth %d < 6 (%s)", straddle.Name, reorg.depth, reorg.String())
	}
	return verdictPass, fmt.Sprintf("straddle op %s spans fork %s, victim's fork block orphaned, dropped branch depth %d (%s)",
		straddle.Name, fork.Format(time.RFC3339), reorg.depth, reorg.String())
}

// forkBlockRecord is one node's view of the fork block: height and hash.
type forkBlockRecord struct {
	number uint64
	hash   string
}

// finalForkBlock returns the istar-final record all nodes agree on, else the latest
// agreed istar record; disagreement or no record is an error.
func (v *verifier) finalForkBlock() (forkBlockRecord, error) {
	final := map[string]forkBlockRecord{}
	latest := map[string]forkBlockRecord{}
	for _, ev := range v.monitor {
		switch ev.Kind {
		case migmon.EvIStar, migmon.EvIStarReorged:
			latest[ev.Node] = forkBlockRecord{number: ev.Number, hash: ev.Hash}
		case migmon.EvIStarFinal:
			final[ev.Node] = forkBlockRecord{number: ev.Number, hash: ev.Hash}
		}
	}
	records, source := final, "istar-final"
	if len(records) == 0 {
		records, source = latest, "istar"
	}
	if len(records) == 0 {
		return forkBlockRecord{}, fmt.Errorf("no istar record observed to establish a final fork block")
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

// checkForkBlockConvergence asserts every node serves the final fork block's hash
// and no no-convergence critical goes unwaived (waiver windows, ±30s).
// INCONCLUSIVE when < 2 distinct provisional fork-block hashes were observed.
func (v *verifier) checkForkBlockConvergence(ctx context.Context) (verdict, string) {
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

	windows := v.waiverWindows()
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
			problems = append(problems, fmt.Sprintf("unwaived no-convergence at %s (node %s): %s", ev.Time.Format(time.RFC3339), ev.Node, ev.Detail))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}

	provisional := map[string]bool{}
	for _, ev := range v.monitor {
		if (ev.Kind == migmon.EvIStar || ev.Kind == migmon.EvIStarReorged) && ev.Hash != "" {
			provisional[ev.Hash] = true
		}
	}
	if len(provisional) < 2 {
		return verdictInconclusive, fmt.Sprintf("all %d node(s) agree on final fork block %d (%s), 0 unwaived no-convergence, but only %d distinct provisional fork-block hash(es) observed, need >= 2 to prove two branches crossed independently",
			len(v.els), final.number, final.hash, len(provisional))
	}
	return verdictPass, fmt.Sprintf("all %d node(s) agree on final fork block %d (%s), 0 unwaived no-convergence, %d distinct provisional fork-block hashes observed",
		len(v.els), final.number, final.hash, len(provisional))
}

// orphanBlock is one post-fork block an istar-reorged event shows a node abandoned.
type orphanBlock struct {
	height uint64
	hash   string
}

var orphanHashRe = regexp.MustCompile(`0x[0-9a-fA-F]{64}`)

// orphanedForkBlocks collects the old hash and height from each istar-reorged event's Detail.
func (v *verifier) orphanedForkBlocks() []orphanBlock {
	var out []orphanBlock
	seen := map[string]bool{}
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvIStarReorged {
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

// checkOrphanGone asserts every orphaned fork-block hash is null or non-canonical on
// every node. INCONCLUSIVE when no istar-reorged event yields an orphan hash.
func (v *verifier) checkOrphanGone(ctx context.Context) (verdict, string) {
	orphans := v.orphanedForkBlocks()
	if len(orphans) == 0 {
		return verdictInconclusive, "no orphaned post-fork block identified (no istar-reorged event with a parsable old hash)"
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

// grepELLogs searches only the execution clients' own log files.
func (v *verifier) grepELLogs(re *regexp.Regexp) (bool, string) {
	for _, node := range v.elNames() {
		for _, f := range v.victimLogFiles(node) {
			if fileContainsMatch(f, re) {
				return true, f
			}
		}
	}
	return false, ""
}
