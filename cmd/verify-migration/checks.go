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
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// verifier holds every input the checks read, plus the resolved b*. It is
// built once by main and never mutated concurrently: checks run in sequence.
type verifier struct {
	els       []el
	T         uint64 // --binary-trie-time
	monitor   []migmon.Event
	chaos     []migmon.Event
	logsDir   string
	pins      map[string]string
	pinsErr   error
	skipChaos bool
	smoke     bool
	fetch     fetcher

	bstar    bstarResult
	bstarErr error
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev migmon.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
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

var reorgDepthRe = regexp.MustCompile(`\(depth (\d+)\)`)
var reorgDropRe = regexp.MustCompile(`Chain reorg detected.*\bdrop=(\d+)`)
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

// matchReorg looks for evidence that w's isolation actually caused a
// reorg on the victim: a monitor reorg event of depth >= 1 within the
// window (+30s slop), or a geth "Chain reorg detected ... drop=N" line in
// the victim's log - drop is the dropped-branch length, exactly the depth
// C3 counts, so log evidence can satisfy the >= 10 requirement too.
func (v *verifier) matchReorg(w chaosWindow) (depth int, ok bool) {
	const slop = 30 * time.Second
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
		if d >= 1 && d > depth {
			depth, ok = d, true
		}
	}
	if v.logsDir != "" {
		for _, f := range v.victimLogFiles(w.node) {
			for _, m := range fileAllMatches(f, reorgDropRe) {
				d, _ := strconv.Atoi(m)
				if d >= 1 && d > depth {
					depth, ok = d, true
				}
			}
		}
	}
	return depth, ok
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

// fileAllMatches returns the first submatch of every re match in f.
func fileAllMatches(f string, re *regexp.Regexp) []string {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		out = append(out, m[1])
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
func (v *verifier) checkC1(ctx context.Context) (bool, string) {
	if v.bstarErr != nil {
		return false, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if v.bstar.number < 101 {
		return false, fmt.Sprintf("b*=%d, need >= 101 for 100 canonical blocks before it", v.bstar.number)
	}
	return true, v.bstar.evidence
}

// checkC2 requires an early first transaction (s0 <= 50) and at least one
// transacting block in every 25-block bucket of [s0, b*-1].
func (v *verifier) checkC2(ctx context.Context) (bool, string) {
	if v.bstarErr != nil {
		return false, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if len(v.els) == 0 {
		return false, "no --el configured"
	}
	e := v.els[0]

	var s0 uint64
	found := false
	for n := uint64(0); n <= 50; n++ {
		cnt, err := v.txCount(ctx, e, n)
		if err != nil {
			return false, fmt.Sprintf("tx count for block %d: %v", n, err)
		}
		if cnt > 0 {
			s0, found = n, true
			break
		}
	}
	if !found {
		return false, "no block with >=1 tx found in [0,50]"
	}
	if v.bstar.number == 0 || v.bstar.number-1 < s0 {
		return false, fmt.Sprintf("s0=%d is not before b*-1 (b*=%d)", s0, v.bstar.number)
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
				return false, fmt.Sprintf("tx count for block %d: %v", n, err)
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
		return false, fmt.Sprintf("s0=%d but empty 25-block bucket(s): %s", s0, strings.Join(emptyBuckets, ","))
	}
	return true, fmt.Sprintf("s0=%d, every 25-block bucket in [%d,%d] has >=1 tx block", s0, s0, end)
}

// checkC3 requires >=4 healed chaos isolations corroborated by a reorg
// (monitor event or victim log line), at least one of depth >= 10, all
// healed no later than T-300. The floor is a COUNT of matched
// isolations, not all-must-match: a short window can legitimately heal
// without an observable reorg when the victim proposed nothing alone.
func (v *verifier) checkC3(ctx context.Context) (bool, string) {
	if v.skipChaos {
		return true, "skipped (--skip-chaos)"
	}
	var healed []chaosWindow
	for _, w := range v.chaosWindows() {
		if w.healed {
			healed = append(healed, w)
		}
	}
	if len(healed) < 4 {
		return false, fmt.Sprintf("only %d healed isolation(s), need >= 4", len(healed))
	}

	deadline := time.Unix(int64(v.T), 0).Add(-300 * time.Second)
	anyDeep := false
	matched := 0
	var problems, notes []string
	for i, w := range healed {
		if w.to.After(deadline) {
			problems = append(problems, fmt.Sprintf("isolation #%d (%s) healed at %s, after T-300 deadline %s",
				i, w.node, w.to.Format(time.RFC3339), deadline.Format(time.RFC3339)))
			continue
		}
		depth, ok := v.matchReorg(w)
		if !ok {
			notes = append(notes, fmt.Sprintf("isolation #%d (%s) unmatched", i, w.node))
			continue
		}
		matched++
		if depth >= 10 {
			anyDeep = true
		}
	}
	if len(problems) > 0 {
		return false, strings.Join(problems, "; ")
	}
	if matched < 4 {
		return false, fmt.Sprintf("only %d isolation(s) matched by a reorg, need >= 4 (%s)", matched, strings.Join(notes, "; "))
	}
	if !anyDeep {
		return false, fmt.Sprintf("%d isolation(s) matched but none reached reorg depth >= 10", matched)
	}
	evidence := fmt.Sprintf("%d/%d healed isolations matched, >=1 at depth >= 10, all healed before T-300", matched, len(healed))
	if len(notes) > 0 {
		evidence += " (" + strings.Join(notes, "; ") + ")"
	}
	return true, evidence
}

// checkC4 requires 3-way (or self-consistent, under --smoke) agreement on
// (hash, stateRoot) for the trailing 32 blocks before b* and for
// [b*-1, b*+10].
func (v *verifier) checkC4(ctx context.Context) (bool, string) {
	if v.bstarErr != nil {
		return false, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	if len(v.els) == 0 {
		return false, "no --el configured"
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
		return false, strings.Join(problems, "; ")
	}
	return true, fmt.Sprintf("%d block(s) in [%d,%d] agree across %d node(s)", checked, lo, hi, len(v.els))
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
func (v *verifier) checkC5(ctx context.Context) (bool, string) {
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
	return len(problems) == 0, evidence
}

// checkC6 applies F1 semantics to the sample stream: full mode wants >=50
// all-non-null-equal samples with zero mismatches and >=10 of those
// strictly outside chaos windows; --smoke relaxes the sample floor. Any
// critical F1 fails unconditionally; other criticals are waived only when
// fully inside a chaos window (+30s slop) for that event's node.
func (v *verifier) checkC6(ctx context.Context) (bool, string) {
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
		return false, strings.Join(problems, "; ")
	}
	return true, fmt.Sprintf("%d samples, %d good, %d outside chaos windows, 0 unwaived criticals", samples, good, outsideGood)
}

// checkC7 compares the pins file against every node's real genesis block.
// genesis_state_root is the required pin: the alloc is what must be stable
// run to run. genesis_hash is optional and asserted only when present -
// kurtosis stamps each run's genesis timestamp at render time, so the hash
// is per-run by design: kurtosis stamps it at render time. "pending" fails, except
// under --smoke where it is a warning.
func (v *verifier) checkC7(ctx context.Context) (bool, string) {
	if v.pinsErr != nil {
		return false, fmt.Sprintf("pins: %v", v.pinsErr)
	}
	hash, hok := v.pins["genesis_hash"]
	root, rok := v.pins["genesis_state_root"]
	if !rok {
		return false, "pins file missing genesis_state_root"
	}
	if (hok && hash == "pending") || root == "pending" {
		if v.smoke {
			return true, "WARN: genesis pins are 'pending' (allowed under --smoke)"
		}
		return false, "genesis pins are 'pending'"
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
		return false, strings.Join(problems, "; ")
	}
	what := "stateRoot"
	if hok {
		what = "hash/stateRoot"
	}
	return true, fmt.Sprintf("genesis %s match pins across %d node(s)", what, len(v.els))
}

var digestLineRe = regexp.MustCompile(`PBT_ARTIFACT_DIGESTS\s+\S*snapshot=([0-9a-fA-F]{64})\s+\S*preimages=([0-9a-fA-F]{64})`)

type artifactDigests struct{ snapshot, preimages string }

// checkC8 requires exactly one PBT_ARTIFACT_DIGESTS line per EL log file,
// with identical snapshot/preimages digests across every node.
func (v *verifier) checkC8(ctx context.Context) (bool, string) {
	if v.logsDir == "" {
		return false, "no --logs-dir given"
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
		return false, strings.Join(problems, "; ")
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
		return false, strings.Join(problems, "; ")
	}
	return true, fmt.Sprintf("snapshot=%s preimages=%s identical across %d node(s)", first.snapshot, first.preimages, len(perNode))
}
