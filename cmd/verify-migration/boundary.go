package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// doneDeadlineAfterFork bounds how long after the fork a node may take to
// report done. The window closes on finality over b*, which live runs put
// ~9 minutes after the fork (fork at +1800s, done at ~+2690s); the bound
// leaves room for one full pathological recovery (the monitor's 12-minute
// split grace) on top before calling the run stuck.
const doneDeadlineAfterFork = 1500 * time.Second

// sameDonePollSlop tolerates the monitor's own polling granularity when
// ordering "done" against "fork block finalized": both surface through 2s
// polls, so a strict comparison would flake on the tie.
const sameDonePollSlop = 3 * time.Second

// introspectingNodes returns the monitored nodes whose client registry
// entry says they answer the migration introspection RPCs, and the ones
// that do not (named, so checks degrade loudly instead of silently).
func (v *verifier) introspectingNodes() (in []string, out []string) {
	for _, node := range v.progressNodes() {
		if spec, ok := migmon.SpecFor(node); ok && spec.Introspects {
			in = append(in, node)
		} else {
			out = append(out, node)
		}
	}
	sort.Strings(in)
	sort.Strings(out)
	return in, out
}

// checkC13 pins the completion semantics per introspecting node: done must
// arrive exactly once, never before that node's own fork block finalized
// (modulo the shared poll tick), and within a bounded time after the fork.
// "Done before finality" is the defect class where a client closes its
// migration window while the boundary can still reorg out from under it.
func (v *verifier) checkC13(ctx context.Context) (verdict, string) {
	nodes, degraded := v.introspectingNodes()
	if len(nodes) == 0 {
		return verdictFail, "no introspecting node reported progress: nothing can prove the migration completed"
	}

	forkT := time.Unix(int64(v.T), 0)
	var problems, notes []string
	for _, node := range nodes {
		var firstDone, finalAt time.Time
		for _, ev := range v.monitor {
			if ev.Node != node {
				continue
			}
			switch ev.Kind {
			case migmon.EvProgress:
				if firstDone.IsZero() && len(ev.Raw) > 0 {
					if p, err := migmon.DecodeProgress(ev.Raw); err == nil && p.Phase == migmon.PhaseDone {
						firstDone = ev.Time
					}
				}
			case migmon.EvBStarFinal:
				if finalAt.IsZero() {
					finalAt = ev.Time
				}
			}
		}
		switch {
		case firstDone.IsZero():
			problems = append(problems, fmt.Sprintf("%s never reported done", node))
		case finalAt.IsZero():
			problems = append(problems, fmt.Sprintf("%s reported done at %s but its fork block never finalized in the observed stream",
				node, firstDone.Format(time.RFC3339)))
		case firstDone.Before(finalAt.Add(-sameDonePollSlop)):
			problems = append(problems, fmt.Sprintf("%s reported done at %s, %.0fs BEFORE its fork block finalized at %s",
				node, firstDone.Format(time.RFC3339), finalAt.Sub(firstDone).Seconds(), finalAt.Format(time.RFC3339)))
		case firstDone.After(forkT.Add(doneDeadlineAfterFork)):
			problems = append(problems, fmt.Sprintf("%s reported done at %s, more than %s after the fork",
				node, firstDone.Format(time.RFC3339), doneDeadlineAfterFork))
		default:
			notes = append(notes, fmt.Sprintf("%s done %.0fs after fork, %.0fs after its fork block finalized",
				node, firstDone.Sub(forkT).Seconds(), firstDone.Sub(finalAt).Seconds()))
		}
	}
	for _, node := range degraded {
		notes = append(notes, fmt.Sprintf("%s: no introspection contract, completion unjudged", node))
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, strings.Join(notes, "; ")
}

// checkC14 hunts direction resurrection: once a node reports done its
// migration machinery must stay closed - a Binary or Merkle direction
// coming back to following/synced, or a non-null shadow root, after done
// means a post-completion event (the gate's own reorg op, cadence chaos)
// illegally reopened the closed window.
func (v *verifier) checkC14(ctx context.Context) (verdict, string) {
	nodes, _ := v.introspectingNodes()
	if len(nodes) == 0 {
		return verdictInconclusive, "no introspecting nodes"
	}
	var problems []string
	scanned := 0
	for _, node := range nodes {
		var doneAt time.Time
		for _, ev := range v.monitor {
			if ev.Node != node || ev.Kind != migmon.EvProgress || len(ev.Raw) == 0 {
				continue
			}
			p, err := migmon.DecodeProgress(ev.Raw)
			if err != nil {
				continue
			}
			if doneAt.IsZero() {
				if p.Phase == migmon.PhaseDone {
					doneAt = ev.Time
				}
				continue
			}
			scanned++
			if migmon.Active(p.Binary) || migmon.Active(p.Merkle) {
				problems = append(problems, fmt.Sprintf("%s: a migration direction is active again at %s, after done at %s",
					node, ev.Time.Format(time.RFC3339), doneAt.Format(time.RFC3339)))
				break
			}
			if p.Binary != nil && p.Binary.ShadowRoot != "" {
				problems = append(problems, fmt.Sprintf("%s: non-null shadow root at %s, after done at %s",
					node, ev.Time.Format(time.RFC3339), doneAt.Format(time.RFC3339)))
				break
			}
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	if scanned == 0 {
		return verdictInconclusive, "no post-done progress events observed"
	}
	return verdictPass, fmt.Sprintf("%d post-done progress polls across %d node(s), no direction resurrection", scanned, len(nodes))
}

// checkC16 judges the run's end state after the post-switchover suite, at
// the strongest anchor the chain itself provides: the lowest finalized
// height across nodes. Every node must hold the same (hash, stateRoot)
// there, and finality must have advanced meaningfully past b* - proof the
// post-fork phase ran ON the binary-canonical chain, not merely near it.
// Requires the lap to have quiesced chaos first (manifest), because an
// end state sampled under live partitions measures the chaos driver.
func (v *verifier) checkC16(ctx context.Context) (verdict, string) {
	if v.manifest == nil || v.manifest.QuiescedAt == 0 {
		return verdictInconclusive, "no quiesce recorded in the manifest; end state was never judged in a quiet network"
	}
	if v.bstarErr != nil {
		return verdictFail, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	minFinal := uint64(0)
	set := false
	var problems []string
	for _, e := range v.els {
		blk, err := v.getBlock(ctx, e, "finalized")
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: finalized: %v", e.name, err))
			continue
		}
		n := uint64(blk.Number)
		if !set || n < minFinal {
			minFinal, set = n, true
		}
	}
	if !set {
		return verdictFail, strings.Join(problems, "; ")
	}
	const beyond = 8
	if minFinal < v.bstar.number+beyond {
		problems = append(problems, fmt.Sprintf("lowest finalized height %d has not advanced %d past b*=%d: the post-fork phase never demonstrably ran on finalized binary-canonical chain",
			minFinal, beyond, v.bstar.number))
	}
	var refHash, refRoot, refNode string
	for _, e := range v.els {
		blk, err := v.getBlock(ctx, e, hexutil.EncodeUint64(minFinal))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: block %d: %v", e.name, minFinal, err))
			continue
		}
		if refNode == "" {
			refHash, refRoot, refNode = blk.Hash.Hex(), blk.StateRoot.Hex(), e.name
			continue
		}
		if !strings.EqualFold(blk.Hash.Hex(), refHash) || !strings.EqualFold(blk.StateRoot.Hex(), refRoot) {
			problems = append(problems, fmt.Sprintf("%s disagrees with %s on (hash, stateRoot) at finalized height %d", e.name, refNode, minFinal))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("all %d node(s) agree at lowest finalized height %d (b*+%d or more), judged after quiesce",
		len(v.els), minFinal, minFinal-v.bstar.number)
}

// checkC17 is the post-boundary sampling hygiene check: cross-node shadow
// root mismatches at heights >= b* are only WARNs in the monitor (post-b*
// sample semantics are not spec-pinned yet), which means nothing FAILS on
// them unless something asks. This asks: any such warning outside a chaos
// window is a finding a green run must not bury.
func (v *verifier) checkC17(ctx context.Context) (verdict, string) {
	if v.bstarErr != nil {
		return verdictFail, fmt.Sprintf("b* unresolved: %v", v.bstarErr)
	}
	postSamples := 0
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvSample && ev.Number >= v.bstar.number {
			postSamples++
		}
	}
	if postSamples == 0 {
		return verdictInconclusive, "no cross-node samples at or past b*"
	}
	windows := v.waiverWindows()
	const slop = 30 * time.Second
	var problems []string
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvWarn || ev.Finding != migmon.FindingRootMismatch {
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
			problems = append(problems, fmt.Sprintf("post-b* root mismatch at %s outside any chaos window: %s", ev.Time.Format(time.RFC3339), ev.Detail))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("%d post-b* sample(s), zero unwaived root-mismatch warnings", postSamples)
}

// walkOrphanBranch measures a dropped branch first-hand instead of trusting
// any log line's arithmetic: starting from the orphaned fork block's hash
// (recorded by the monitor's bstar-reorged event), walk parent links on the
// victim's own RPC until the walked block matches the canonical hash at its
// height - that join is the common ancestor. The branch tip is the highest
// head the monitor recorded for the victim inside the window, so
// depth = tip - ancestor. Only meaningful on clients that keep serving
// orphaned blocks by hash (registry ServesOrphans).
func (v *verifier) walkOrphanBranch(ctx context.Context, victim el, orphanHash string, tipHint uint64) (depth, ancestor int, err error) {
	hash := orphanHash
	for step := 0; step < 128; step++ {
		blk, err := v.getBlockByHash(ctx, victim, hash)
		if err != nil {
			return 0, 0, err
		}
		if blk == nil {
			return 0, 0, fmt.Errorf("victim no longer serves %s", hash)
		}
		height := uint64(blk.Number)
		canon, err := v.getBlock(ctx, victim, hexutil.EncodeUint64(height))
		if err != nil {
			return 0, 0, err
		}
		if strings.EqualFold(canon.Hash.Hex(), blk.Hash.Hex()) {
			if tipHint <= height {
				return 0, int(height), fmt.Errorf("victim tip hint %d at or below ancestor %d", tipHint, height)
			}
			return int(tipHint - height), int(height), nil
		}
		hash = blk.ParentHash.Hex()
	}
	return 0, 0, fmt.Errorf("no canonical join within 128 parent steps of %s", orphanHash)
}

// victimTip returns the highest head the monitor recorded for the victim
// inside [from, to]: the tip of the branch the heal threw away.
func (v *verifier) victimTip(node string, from, to time.Time) uint64 {
	var tip uint64
	for _, ev := range v.monitor {
		if ev.Kind != migmon.EvHead || !sameNode(ev.Node, node) {
			continue
		}
		if ev.Time.Before(from) || ev.Time.After(to) {
			continue
		}
		if ev.Number > tip {
			tip = ev.Number
		}
	}
	return tip
}
