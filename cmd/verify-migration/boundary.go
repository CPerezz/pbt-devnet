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

// doneDeadlineAfterFork bounds how long after the fork a node may take to report done.
const doneDeadlineAfterFork = 1500 * time.Second

// sameDonePollSlop tolerates the monitor's 2s polling granularity when ordering
// "done" against "fork block finalized".
const sameDonePollSlop = 3 * time.Second

// introspectingNodes splits monitored nodes by whether their client registry
// entry answers the migration introspection RPCs.
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

// checkCompletion asserts each introspecting node's merkle direction started only
// at/after T, done arrived after its fork block finalized (± sameDonePollSlop)
// and within doneDeadlineAfterFork, and nothing reopens after done.
func (v *verifier) checkCompletion(ctx context.Context) (verdict, string) {
	nodes, degraded := v.introspectingNodes()
	if len(nodes) == 0 {
		return verdictFail, "no introspecting node reported progress: nothing can prove the migration completed"
	}

	forkT := time.Unix(int64(v.T), 0)
	var problems, notes []string
	for _, node := range nodes {
		var firstDone, finalAt time.Time
		postDone := 0
		for _, ev := range v.monitor {
			if ev.Node != node {
				continue
			}
			if ev.Kind == migmon.EvIStarFinal && finalAt.IsZero() {
				finalAt = ev.Time
			}
			if ev.Kind != migmon.EvProgress || len(ev.Raw) == 0 {
				continue
			}
			p, err := migmon.DecodeProgress(ev.Raw)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: undecodable progress at %s", node, ev.Time.Format(time.RFC3339)))
				break
			}
			if firstDone.IsZero() {
				if migmon.Active(p.Merkle) && ev.Time.Before(forkT) {
					problems = append(problems, fmt.Sprintf("%s: merkle direction active at %s, before T", node, ev.Time.Format(time.RFC3339)))
					break
				}
				if p.Phase == migmon.PhaseDone {
					firstDone = ev.Time
				}
				continue
			}
			postDone++
			switch {
			case p.Phase != migmon.PhaseDone:
				problems = append(problems, fmt.Sprintf("%s: regressed from done to %q at %s", node, p.Phase, ev.Time.Format(time.RFC3339)))
			case migmon.Active(p.Binary) || migmon.Active(p.Merkle):
				problems = append(problems, fmt.Sprintf("%s: a direction is active again at %s, after done", node, ev.Time.Format(time.RFC3339)))
			case p.Binary != nil && p.Binary.ShadowRoot != "":
				problems = append(problems, fmt.Sprintf("%s: non-null shadow root at %s, after done", node, ev.Time.Format(time.RFC3339)))
			default:
				continue
			}
			break
		}
		switch {
		case firstDone.IsZero():
			problems = append(problems, fmt.Sprintf("%s never reported done", node))
		case finalAt.IsZero():
			problems = append(problems, fmt.Sprintf("%s reported done at %s but its fork block never finalized", node, firstDone.Format(time.RFC3339)))
		case firstDone.Before(finalAt.Add(-sameDonePollSlop)):
			problems = append(problems, fmt.Sprintf("%s reported done %.0fs BEFORE its fork block finalized", node, finalAt.Sub(firstDone).Seconds()))
		case firstDone.After(forkT.Add(doneDeadlineAfterFork)):
			problems = append(problems, fmt.Sprintf("%s reported done more than %s after the fork", node, doneDeadlineAfterFork))
		default:
			notes = append(notes, fmt.Sprintf("%s done %.0fs after fork, %.0fs after its fork block finalized, %d clean post-done polls",
				node, firstDone.Sub(forkT).Seconds(), firstDone.Sub(finalAt).Seconds(), postDone))
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

// checkFinalizedEndState asserts every node agrees on (hash, stateRoot) at the
// lowest finalized height, and that height is >= I*+8. INCONCLUSIVE if the
// manifest never recorded a quiesce.
func (v *verifier) checkFinalizedEndState(ctx context.Context) (verdict, string) {
	if v.manifest == nil || v.manifest.QuiescedAt == 0 {
		return verdictInconclusive, "no quiesce recorded in the manifest; end state was never judged in a quiet network"
	}
	if v.istarErr != nil {
		return verdictFail, fmt.Sprintf("I* unresolved: %v", v.istarErr)
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
	if minFinal < v.istar.number+beyond {
		problems = append(problems, fmt.Sprintf("lowest finalized height %d has not advanced %d past I*=%d: the post-fork phase never demonstrably ran on finalized binary-canonical chain",
			minFinal, beyond, v.istar.number))
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
	return verdictPass, fmt.Sprintf("all %d node(s) agree at lowest finalized height %d (I*+%d or more), judged after quiesce",
		len(v.els), minFinal, minFinal-v.istar.number)
}

// checkPostForkSamples asserts zero unwaived post-I* root-mismatch warnings
// (waiver windows, ±30s). INCONCLUSIVE with no post-I* samples.
func (v *verifier) checkPostForkSamples(ctx context.Context) (verdict, string) {
	if v.istarErr != nil {
		return verdictFail, fmt.Sprintf("I* unresolved: %v", v.istarErr)
	}
	postSamples := 0
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvSample && ev.Number >= v.istar.number {
			postSamples++
		}
	}
	if postSamples == 0 {
		return verdictInconclusive, "no cross-node samples at or past I*"
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
			problems = append(problems, fmt.Sprintf("post-I* root mismatch at %s outside any chaos window: %s", ev.Time.Format(time.RFC3339), ev.Detail))
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	return verdictPass, fmt.Sprintf("%d post-I* sample(s), zero unwaived root-mismatch warnings", postSamples)
}

// walkOrphanBranch walks parent links from the orphaned fork block's hash until
// it joins the canonical chain; depth = victim's recorded tip - join height.
// Only meaningful on clients that keep serving orphaned blocks by hash.
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

// victimTip returns the highest head the monitor recorded for node inside [from, to].
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
