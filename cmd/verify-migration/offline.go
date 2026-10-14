// Offline migration lap checks: a non-validating geth and a live erigon
// export real EIP-8347 artifacts, nethermind and geth each import the
// other's artifacts in turn, then replay BALs to head before the fork.
// Every check here is INCONCLUSIVE when the manifest carries no `offline`
// block or `offline.enabled` is false, so an online lap is unaffected.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/pbtsnap"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// requireOffline resolves the manifest for an offline-lap check: a nil
// manifest means the caller should return (verdict, note) as-is.
func (v *verifier) requireOffline() (*lapManifest, verdict, string) {
	if v.manifestPath == "" {
		return nil, verdictInconclusive, "no --manifest given (run driven outside the lap script)"
	}
	if v.manifestErr != nil {
		return nil, verdictInconclusive, fmt.Sprintf("manifest unreadable (see lap-manifest): %v", v.manifestErr)
	}
	if v.manifest.Offline == nil || !v.manifest.Offline.Enabled {
		return nil, verdictInconclusive, "no offline lap configured (offline missing or disabled)"
	}
	return v.manifest, verdictPass, ""
}

// artifactPath resolves a producer/swap-relative path under the manifest's directory ($OUT).
func (v *verifier) artifactPath(relDir, name string) string {
	return filepath.Join(v.artifactsDir, relDir, name)
}

// readErigonMetaRoot reads pbtRoot from an erigon `export-pbt` meta.json.
func readErigonMetaRoot(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var meta struct {
		PbtRoot string `json:"pbtRoot"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if meta.PbtRoot == "" {
		return "", fmt.Errorf("%s: no pbtRoot field", path)
	}
	return meta.PbtRoot, nil
}

// checkArtifactProduced asserts, per producer, that the snapshot trailer, the
// client's own log/meta claim, the cross-client shadow-root samples and a
// live RPC read of the anchor all agree on one pre-fork, post-genesis root.
func (v *verifier) checkArtifactProduced(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}
	if len(m.Producers) == 0 {
		return verdictInconclusive, "offline lap enabled but no producer evidence recorded"
	}

	var problems, notes []string
	for _, p := range m.Producers {
		var pp []string
		if p.Status != "ok" {
			problems = append(problems, fmt.Sprintf("producer node %d: status %s, want ok: %s", p.Node, p.Status, p.Detail))
			continue
		}

		snapRoot, err := pbtsnap.Root(v.artifactPath(p.Dir, "pbt-snapshot.bin"))
		if err != nil {
			pp = append(pp, fmt.Sprintf("snapshot trailer: %v", err))
		} else if !strings.EqualFold(snapRoot.Hex(), p.PbtRoot) {
			pp = append(pp, fmt.Sprintf("snapshot trailer %s != manifest pbt_root %s", snapRoot.Hex(), p.PbtRoot))
		}

		switch p.Kind {
		case "geth-convert":
			if raw, err := os.ReadFile(v.artifactPath(p.Dir, "convert.log")); err != nil {
				pp = append(pp, fmt.Sprintf("convert.log: %v", err))
			} else if _, root, err := pbtsnap.ConvertLog(string(raw)); err != nil {
				pp = append(pp, fmt.Sprintf("convert.log: %v", err))
			} else if !pbtsnap.LoggedMatches(root, common.HexToHash(p.PbtRoot)) {
				pp = append(pp, fmt.Sprintf("convert.log binaryRoot %s != manifest pbt_root %s", root, p.PbtRoot))
			}
		case "erigon-export":
			if root, err := readErigonMetaRoot(v.artifactPath(p.Dir, "pbt-snapshot.meta.json")); err != nil {
				pp = append(pp, fmt.Sprintf("meta.json: %v", err))
			} else if !strings.EqualFold(root, p.PbtRoot) {
				pp = append(pp, fmt.Sprintf("meta.json pbtRoot %s != manifest pbt_root %s", root, p.PbtRoot))
			}
		default:
			pp = append(pp, fmt.Sprintf("unknown producer kind %q", p.Kind))
		}

		nonEmpty, distinctClients := 0, map[string]bool{}
		for node, root := range p.ShadowRoots {
			if root == "" {
				continue
			}
			nonEmpty++
			if !strings.EqualFold(root, p.PbtRoot) {
				pp = append(pp, fmt.Sprintf("shadow root from %s is %s, != pbt_root %s", node, root, p.PbtRoot))
				continue
			}
			client := clientOf(node)
			if client == "" {
				client = node
			}
			distinctClients[client] = true
		}
		if len(distinctClients) < 2 {
			pp = append(pp, fmt.Sprintf("only %d distinct client(s) reported a matching shadow root (%d non-empty total), need >= 2", len(distinctClients), nonEmpty))
		}

		if v.pinsErr != nil {
			pp = append(pp, fmt.Sprintf("pins: %v", v.pinsErr))
		} else if strings.EqualFold(p.AnchorStateRoot, v.pins["genesis_state_root"]) {
			pp = append(pp, "anchor_state_root equals genesis_state_root: proves nothing migrated past genesis")
		}

		if e, ok := v.elByIndex(p.Node); !ok {
			pp = append(pp, fmt.Sprintf("no --el for producer node %d: cannot confirm the anchor is canonical", p.Node))
		} else if blk, err := v.getBlock(ctx, e, hexutil.EncodeUint64(p.Anchor)); err != nil {
			pp = append(pp, fmt.Sprintf("%s: %v", e.name, err))
		} else {
			if !strings.EqualFold(blk.Hash.Hex(), p.AnchorHash) {
				pp = append(pp, fmt.Sprintf("%s canonical block %d hash %s != manifest anchor_hash %s", e.name, p.Anchor, blk.Hash.Hex(), p.AnchorHash))
			}
			if uint64(blk.Timestamp) >= v.T {
				pp = append(pp, fmt.Sprintf("%s anchor %d header time %d >= binary-trie-time %d: not a pre-fork anchor", e.name, p.Anchor, uint64(blk.Timestamp), v.T))
			}
		}

		if len(pp) > 0 {
			problems = append(problems, fmt.Sprintf("producer node %d: %s", p.Node, strings.Join(pp, "; ")))
			continue
		}
		notes = append(notes, fmt.Sprintf("producer node %d (%s) anchor %d root %s: trailer/log/shadow/RPC agree", p.Node, p.Kind, p.Anchor, p.PbtRoot))
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}

var (
	// geth logs its roots abbreviated (see pbtsnap.LoggedMatches).
	gethImportCompleteRe  = regexp.MustCompile(`Import complete.*\bbinaryRoot=(\S+).*\banchor=(\d+)`)
	nethermindMigrationRe = regexp.MustCompile(`PBT state at StateId \{ BlockNumber = (\d+),`)
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// swapBoot is the part of a consumer's container log the swap's own boot wrote. Nethermind
// colours its log, and the container's earlier boot (seeded from genesis) logged a success
// line of its own at block 0; the wrapper marks where the swap's boot begins. The first
// marker: a later restart reuses the import and logs alignment at its own head.
func swapBoot(raw []byte) (string, bool) {
	text := ansiRe.ReplaceAllString(string(raw), "")
	i := strings.Index(text, "pbt-swap: starting")
	if i < 0 {
		return "", false
	}
	return text[i:], true
}

// nethermindRefusals are the client's own refusal log lines; any of
// these in the evidence means the import never landed cleanly.
var nethermindRefusals = []string{
	"Migration anchor is not present in the trusted canonical chain",
	"Migration requires a trusted pre-activation anchor",
	"The native PBT database was imported from another migration source",
	"native PBT database holds no state",
}

// checkImportAccepted asserts, per swap, that the consumer's own log proves
// a clean import of the manifest's anchor: geth's online import (see
// gethOnlineImport, and the monitor's view of it), or nethermind's
// migration-bootstrap success line naming the anchor with none of its refusal
// strings present. Either way the anchor must not be ahead of node 1's
// finalized height at the stop.
func (v *verifier) checkImportAccepted(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}
	if len(m.Swaps) == 0 {
		return verdictInconclusive, "offline lap enabled but no swaps recorded"
	}

	var problems, notes []string
	for _, s := range m.Swaps {
		var pp []string
		client := v.victimClient(fmt.Sprintf("node-%d", s.Node))
		if s.Status == migmon.StepSkipped {
			problems = append(problems, fmt.Sprintf("consumer node %d (%s): swap skipped: %s", s.Node, client, s.Detail))
			continue
		}
		switch client {
		case "geth":
			raw, err := os.ReadFile(v.artifactPath(s.EvidenceDir, "docker.log"))
			if err != nil {
				pp = append(pp, fmt.Sprintf("docker.log: %v", err))
				break
			}
			text, ok := swapBoot(raw)
			if !ok {
				pp = append(pp, `no "pbt-swap: starting" line: no swap boot to judge`)
				break
			}
			if s.Mode == "convert" {
				pp = append(pp, gethOnlineConvert(text, s)...)
			} else {
				pp = append(pp, gethOnlineImport(text, s, m.Producers)...)
			}
			followed, note := v.followedThroughImport(s)
			pp = append(pp, followed...)
			notes = append(notes, fmt.Sprintf("consumer node %d (geth) %s", s.Node, note))
		case "nethermind":
			raw, err := os.ReadFile(v.artifactPath(s.EvidenceDir, "docker.log"))
			if err != nil {
				pp = append(pp, fmt.Sprintf("docker.log: %v", err))
				break
			}
			text, ok := swapBoot(raw)
			if !ok {
				pp = append(pp, `no "pbt-swap: starting" line: no swap boot to judge`)
				break
			}
			for _, needle := range nethermindRefusals {
				if strings.Contains(text, needle) {
					pp = append(pp, fmt.Sprintf("refusal logged: %q", needle))
				}
			}
			if mm := nethermindMigrationRe.FindStringSubmatch(text); mm == nil {
				pp = append(pp, `no "EIP-8347 migration: flat state at ..., PBT state at ..." success line`)
			} else if anchor, _ := strconv.ParseUint(mm[1], 10, 64); anchor != s.Anchor {
				pp = append(pp, fmt.Sprintf("PBT StateId block %s != manifest anchor %d", mm[1], s.Anchor))
			}
		default:
			pp = append(pp, fmt.Sprintf("unrecognized consumer client %q for node %d", client, s.Node))
		}

		if s.Anchor > s.FinalizedAtStop {
			pp = append(pp, fmt.Sprintf("anchor %d > finalized_at_stop %d", s.Anchor, s.FinalizedAtStop))
		}

		if len(pp) > 0 {
			problems = append(problems, fmt.Sprintf("consumer node %d (%s): %s", s.Node, client, strings.Join(pp, "; ")))
			continue
		}
		notes = append(notes, fmt.Sprintf("consumer node %d (%s) accepted anchor %d", s.Node, client, s.Anchor))
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}

// gethOnlineImport judges a geth consumer's swap boot. The node starts once
// with the import flags and serves RPC before the import begins, so it keeps
// following the chain while importing. The import ends at the manifest's
// anchor with the producer's root, and nothing is logged at ERROR or CRIT.
func gethOnlineImport(text string, s migmon.SwapRecord, producers []migmon.ProducerRecord) []string {
	var pp []string
	if n := strings.Count(text, "pbt-swap: starting"); n != 1 {
		pp = append(pp, fmt.Sprintf("%d boots with the import flags, want 1: the node restarted after the swap", n))
	}
	served := strings.Index(text, "HTTP server started")
	began := strings.Index(text, "Starting binary tree import in the background")
	switch {
	case began < 0:
		pp = append(pp, "no background import began")
	case served < 0 || served > began:
		pp = append(pp, "the import began before the node served RPC")
	}
	if mm := gethImportCompleteRe.FindStringSubmatch(text); mm == nil {
		pp = append(pp, `no "Import complete ... binaryRoot= ... anchor=" line`)
	} else {
		if anchor, _ := strconv.ParseUint(mm[2], 10, 64); anchor != s.Anchor {
			pp = append(pp, fmt.Sprintf("imported anchor %s != manifest anchor %d", mm[2], s.Anchor))
		}
		if i := slices.IndexFunc(producers, func(p migmon.ProducerRecord) bool { return p.Node == s.Producer }); i < 0 {
			pp = append(pp, fmt.Sprintf("no record of producer node %d", s.Producer))
		} else if !pbtsnap.LoggedMatches(mm[1], common.HexToHash(producers[i].PbtRoot)) {
			pp = append(pp, fmt.Sprintf("imported root %s != producer root %s", mm[1], producers[i].PbtRoot))
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ERROR[") || strings.HasPrefix(line, "CRIT [") {
			pp = append(pp, "logged: "+line)
		}
	}
	return pp
}

// gethConversionCompleteLineRe isolates geth's self-conversion success line; Go's "."
// never crosses a newline, so this already stops at end of line.
var gethConversionCompleteLineRe = regexp.MustCompile(`Conversion complete\b.*`)
var (
	gethConvAnchorRe = regexp.MustCompile(`\banchor=(\d+)`)
	gethConvHashRe   = regexp.MustCompile(`\bhash=(\S+)`)
	gethConvRootRe   = regexp.MustCompile(`\bbinaryRoot=(\S+)`)
)

// gethConversionComplete reads geth's self-conversion success line: "Conversion
// complete" with anchor=<P>, hash=<P's hash>, binaryRoot=<root>, field order not
// assumed. ok is false when the line, or any of its three fields, is missing.
func gethConversionComplete(text string) (anchor uint64, hash, root string, ok bool) {
	line := gethConversionCompleteLineRe.FindString(text)
	if line == "" {
		return 0, "", "", false
	}
	am, hm, rm := gethConvAnchorRe.FindStringSubmatch(line), gethConvHashRe.FindStringSubmatch(line), gethConvRootRe.FindStringSubmatch(line)
	if am == nil || hm == nil || rm == nil {
		return 0, "", "", false
	}
	anchor, _ = strconv.ParseUint(am[1], 10, 64)
	return anchor, hm[1], rm[1], true
}

// gethOnlineConvert judges a geth consumer's swap boot in convert mode: self-conversion,
// fed only the producer's preimages and anchor hash, no snapshot. There is no producer
// root to compare against - the self-converter's root is its own persisted block P, not
// the exporter's anchor A (EIP-8347 l.203 makes replay from A to P bit-identical to
// converting P; checked instead by checkGethConvertShadowFromP's shadow-root agreement).
func gethOnlineConvert(text string, s migmon.SwapRecord) []string {
	var pp []string
	if n := strings.Count(text, "pbt-swap: starting"); n != 1 {
		pp = append(pp, fmt.Sprintf("%d boots with the import flags, want 1: the node restarted after the swap", n))
	}
	if _, _, _, ok := gethConversionComplete(text); !ok {
		pp = append(pp, `no "Conversion complete ... anchor= ... hash= ... binaryRoot=" line`)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ERROR[") || strings.HasPrefix(line, "CRIT [") {
			pp = append(pp, "logged: "+line)
		}
	}
	return pp
}

// checkGethConvertShadowFromP asserts, for every geth consumer swapped in convert mode,
// that from its own converted anchor P on - read from its "Conversion complete" line, not
// the manifest's producer anchor A - its shadow root agrees with every other reporting
// node's at the same monitor-sampled block. This is the devnet's stand-in for "root =
// producer's root": the self-converter is judged against its peers at its own anchor
// instead (plan.md §9, "the devnet proof is shadow-root agreement from P on").
func (v *verifier) checkGethConvertShadowFromP(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}
	var converters []migmon.SwapRecord
	for _, s := range m.Swaps {
		if s.Mode == "convert" {
			converters = append(converters, s)
		}
	}
	if len(converters) == 0 {
		return verdictInconclusive, "no geth convert-mode swap recorded"
	}

	var problems, notes []string
	for _, s := range converters {
		el, ok := v.elByIndex(s.Node)
		if !ok {
			problems = append(problems, fmt.Sprintf("consumer node %d: no --el: cannot name it in the shadow samples", s.Node))
			continue
		}
		if client := clientOf(el.name); client != "geth" {
			problems = append(problems, fmt.Sprintf("consumer node %d: convert mode recorded for client %q, want geth", s.Node, client))
			continue
		}
		if s.Status == migmon.StepSkipped {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): swap skipped: %s", s.Node, s.Detail))
			continue
		}
		raw, err := os.ReadFile(v.artifactPath(s.EvidenceDir, "docker.log"))
		if err != nil {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): docker.log: %v", s.Node, err))
			continue
		}
		text, ok := swapBoot(raw)
		if !ok {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): no \"pbt-swap: starting\" line: no swap boot to judge", s.Node))
			continue
		}
		p, _, _, ok := gethConversionComplete(text)
		if !ok {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): no \"Conversion complete\" line to read its own anchor P from", s.Node))
			continue
		}

		heights := map[uint64]bool{}
		var mismatches []string
		for _, ev := range v.monitor {
			if ev.Kind != migmon.EvSample || ev.Number < p {
				continue
			}
			mine, ok := ev.Roots[el.name]
			if !ok || mine == "" {
				continue
			}
			others := make([]string, 0, len(ev.Roots))
			for node := range ev.Roots {
				if node != el.name && ev.Roots[node] != "" {
					others = append(others, node)
				}
			}
			sort.Strings(others)
			for _, node := range others {
				heights[ev.Number] = true
				if root := ev.Roots[node]; root != mine {
					mismatches = append(mismatches, fmt.Sprintf("block %d: %s=%s != %s=%s", ev.Number, el.name, mine, node, root))
				}
			}
		}
		if len(mismatches) > 0 {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): %s", s.Node, strings.Join(mismatches, "; ")))
			continue
		}
		if len(heights) == 0 {
			problems = append(problems, fmt.Sprintf("consumer node %d (geth): no monitor shadow sample at or after its own anchor %d to cross-check", s.Node, p))
			continue
		}
		notes = append(notes, fmt.Sprintf("consumer node %d (geth) agrees with every other reporting node at %d sampled block(s) >= its own anchor %d", s.Node, len(heights), p))
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}

// followedThroughImport reads the monitor stream for the consumer's import
// window, from its first "importing" progress sample to the first other one.
// Once its head first comes within CaughtUpLag of node 1's after the restart,
// it must stay there for the rest of the window: the node keeps following
// while it imports.
func (v *verifier) followedThroughImport(s migmon.SwapRecord) ([]string, string) {
	var (
		tip                  uint64
		haveTip, inWindow    bool
		caughtUp             bool
		samples, behind      int
		firstHead, lastHead  uint64
		windowFrom, windowTo time.Time
	)
	start := time.Unix(s.StartAt, 0)
	for _, ev := range v.monitor {
		i, ok := nodeIndex(ev.Node)
		if !ok || ev.Time.Before(start) {
			continue
		}
		if ev.Kind == migmon.EvHead && i == 1 {
			tip, haveTip = ev.Number, true
			continue
		}
		if i != s.Node {
			continue
		}
		if ev.Kind == migmon.EvProgress {
			if ev.Phase == migmon.PhaseImporting {
				if !inWindow {
					windowFrom = ev.Time
				}
				inWindow = true
				windowTo = ev.Time
			} else if inWindow {
				break
			}
			continue
		}
		if ev.Kind != migmon.EvHead || !inWindow || !haveTip {
			continue
		}
		if samples == 0 {
			firstHead = ev.Number
		}
		samples++
		lastHead = ev.Number
		within := ev.Number+migmon.CaughtUpLag >= tip
		if caughtUp && !within {
			behind++
		}
		caughtUp = caughtUp || within
	}
	if samples == 0 {
		return nil, "imported between monitor polls"
	}
	note := fmt.Sprintf("imported over %s, head %d -> %d in %d samples", windowTo.Sub(windowFrom).Round(time.Second), firstHead, lastHead, samples)
	if behind > 0 {
		return []string{fmt.Sprintf("head fell over %d blocks behind node 1 in %d of %d samples while importing", migmon.CaughtUpLag, behind, samples)}, note
	}
	return nil, note
}

// checkSwapsSerialized asserts the consumer swaps ran one at a time, in
// chaos-free gaps, never touching node 1, each within its own timeout.
func (v *verifier) checkSwapsSerialized(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}
	if len(m.Swaps) == 0 {
		return verdictInconclusive, "offline lap enabled but no swaps recorded"
	}

	dump, schedErr := v.scheduleDump()
	var problems []string
	type window struct {
		node       int
		start, end time.Time
	}
	var windows []window
	skipped := 0
	for _, s := range m.Swaps {
		if s.Node == 1 {
			problems = append(problems, "swap targets node 1, the protected anchor")
		}
		// A skipped swap never stopped its node, so it has no window to serialize;
		// offline-coverage fails the lap for the skip itself.
		if s.Status == migmon.StepSkipped {
			skipped++
			continue
		}
		if s.StopAt == 0 {
			problems = append(problems, fmt.Sprintf("node %d: stop_at never recorded", s.Node))
			continue
		}
		endSec := s.CaughtUpAt
		if s.StartAt > endSec {
			endSec = s.StartAt
		}
		if endSec == 0 {
			problems = append(problems, fmt.Sprintf("node %d: neither caught_up_at nor start_at recorded", s.Node))
			continue
		}
		windows = append(windows, window{s.Node, time.Unix(s.StopAt, 0), time.Unix(endSec, 0)})

		if schedErr == nil {
			start, end := time.Unix(s.StopAt, 0), time.Unix(endSec, 0)
			for _, op := range dump.Admitted() {
				opStart := time.Unix(op.Start, 0).Add(-60 * time.Second)
				opEnd := time.Unix(op.End, 0).Add(60 * time.Second)
				if !start.After(opEnd) && !end.Before(opStart) {
					problems = append(problems, fmt.Sprintf("node %d swap window [%s,%s] overlaps admitted op %s [%s,%s] (+-60s)",
						s.Node, start.Format(time.RFC3339), end.Format(time.RFC3339), op.Name, opStart.Format(time.RFC3339), opEnd.Format(time.RFC3339)))
				}
			}
		}

		if s.CaughtUpAt != 0 {
			for _, c := range m.Offline.Consumers {
				if c.Node == s.Node && c.TimeoutSeconds > 0 && s.CaughtUpAt-s.StopAt > c.TimeoutSeconds {
					problems = append(problems, fmt.Sprintf("node %d: caught up %ds after stop, over its %ds timeout",
						s.Node, s.CaughtUpAt-s.StopAt, c.TimeoutSeconds))
				}
			}
		}
	}
	if schedErr != nil && !v.skipChaos {
		problems = append(problems, fmt.Sprintf("schedule: %v", schedErr))
	}

	for i := range windows {
		for j := i + 1; j < len(windows); j++ {
			if windows[i].start.Before(windows[j].end) && windows[j].start.Before(windows[i].end) {
				problems = append(problems, fmt.Sprintf("swap windows for node %d and node %d overlap", windows[i].node, windows[j].node))
			}
		}
	}

	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	if len(windows) == 0 {
		return verdictInconclusive, fmt.Sprintf("all %d swap(s) skipped: nothing ran to serialize", skipped)
	}
	evidence := fmt.Sprintf("%d swap(s) serialized, chaos-free, within budget", len(windows))
	if skipped > 0 {
		evidence += fmt.Sprintf("; %d skipped (never stopped, judged by offline-coverage)", skipped)
	}
	return verdictPass, evidence
}

// checkReplayCaughtUp asserts, per swap, that the consumer caught up with
// enough margin before the fork, corroborated independently by the monitor
// stream (not just the swap driver's own say-so).
func (v *verifier) checkReplayCaughtUp(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}
	if len(m.Swaps) == 0 {
		return verdictInconclusive, "offline lap enabled but no swaps recorded"
	}

	margin := time.Duration(m.Offline.MarginSeconds) * time.Second
	deadline := time.Unix(int64(v.T), 0).Add(-margin)
	var problems, notes []string
	for _, s := range m.Swaps {
		if s.CaughtUpAt == 0 {
			problems = append(problems, fmt.Sprintf("node %d: never caught up", s.Node))
			continue
		}
		caughtUp := time.Unix(s.CaughtUpAt, 0)
		if caughtUp.After(deadline) {
			problems = append(problems, fmt.Sprintf("node %d caught up at %s, after T-margin deadline %s",
				s.Node, caughtUp.Format(time.RFC3339), deadline.Format(time.RFC3339)))
		}
		if !v.lifecycleCaughtUp(s.Node, time.Unix(s.StartAt, 0)) {
			problems = append(problems, fmt.Sprintf("node %d: no independent caught_up corroboration in the monitor stream", s.Node))
			continue
		}
		notes = append(notes, fmt.Sprintf("node %d caught up at %s, corroborated", s.Node, caughtUp.Format(time.RFC3339)))
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}

// lifecycleCaughtUp reports independent monitor corroboration that node
// reached caught_up after t: a monitor-sampled binary-direction cursor within
// CaughtUpLag of the chain tip, node 1's head (the protected anchor). The node's
// own head is no reference: right after a restart it is as stale as the cursor.
func (v *verifier) lifecycleCaughtUp(node int, after time.Time) bool {
	var tip uint64
	haveTip := false
	for _, ev := range v.monitor {
		i, ok := nodeIndex(ev.Node)
		if !ok {
			continue
		}
		switch {
		case ev.Kind == migmon.EvHead && i == 1:
			tip, haveTip = ev.Number, true
		case ev.Kind == migmon.EvProgress && i == node:
			if ev.Time.Before(after) || len(ev.Raw) == 0 || !haveTip {
				continue
			}
			p, err := migmon.DecodeProgress(ev.Raw)
			if err != nil || p.Binary == nil {
				continue
			}
			if uint64(p.Binary.Cursor)+migmon.CaughtUpLag >= tip {
				return true
			}
		}
	}
	return false
}

// checkOfflineCoverage asserts every declared producer and consumer finished
// ok, and reports the online-only nodes as skipped (with reason), not failed.
func (v *verifier) checkOfflineCoverage(ctx context.Context) (verdict, string) {
	m, r, msg := v.requireOffline()
	if m == nil {
		return r, msg
	}

	var problems, notes []string
	for _, p := range m.Offline.Producers {
		i := slices.IndexFunc(m.Producers, func(r migmon.ProducerRecord) bool { return r.Node == p.Node })
		if i < 0 || m.Producers[i].Status != "ok" {
			status := "missing"
			if i >= 0 {
				status = m.Producers[i].Status
			}
			problems = append(problems, fmt.Sprintf("producer node %d (%s): status %s, want ok", p.Node, p.Kind, status))
			continue
		}
		notes = append(notes, fmt.Sprintf("producer node %d (%s) ok", p.Node, p.Kind))
	}
	for _, c := range m.Offline.Consumers {
		i := slices.IndexFunc(m.Swaps, func(s migmon.SwapRecord) bool { return s.Node == c.Node })
		if i < 0 || m.Swaps[i].Status != "ok" {
			status := "missing"
			if i >= 0 {
				status = m.Swaps[i].Status
			}
			problems = append(problems, fmt.Sprintf("consumer node %d: swap status %s, want ok", c.Node, status))
			continue
		}
		notes = append(notes, fmt.Sprintf("consumer node %d swap ok", c.Node))
	}
	for _, o := range m.Offline.OnlineOnly {
		notes = append(notes, fmt.Sprintf("node %d skipped: %s", o.Node, o.Reason))
	}

	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}
