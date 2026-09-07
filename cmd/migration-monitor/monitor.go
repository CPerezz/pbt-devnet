package main

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// reorgRecheckWindow bounds re-fetched heights per sample tick to catch a
// reorg (8: cheap; widen if a scenario needs deeper).
const reorgRecheckWindow = 8

// resampleQueueCap bounds the targeted-resample queue (64: holds several
// concurrent reorg/fork-burst events without growing memory).
const resampleQueueCap = 64

// forkBurstWindow is how far back of I* a final-burst reaches with no
// recorded reorg to anchor it.
const forkBurstWindow = 8

// resampleQueue holds heights a reorg or fork-block event just made
// interesting for the next sample tick: FIFO, deduped, drops the oldest
// once full. Pushed only from main's single select loop, so no lock needed.
type resampleQueue struct {
	heights []uint64
	seen    map[uint64]bool
}

func newResampleQueue() *resampleQueue {
	return &resampleQueue{seen: make(map[uint64]bool)}
}

// push enqueues height for sampling; drops height < 1 and repeats.
func (q *resampleQueue) push(height uint64) {
	if height < 1 || q.seen[height] {
		return
	}
	if len(q.heights) >= resampleQueueCap {
		delete(q.seen, q.heights[0])
		q.heights = q.heights[1:]
	}
	q.heights = append(q.heights, height)
	q.seen[height] = true
}

// pushRange enqueues every height in [lo, hi].
func (q *resampleQueue) pushRange(lo, hi uint64) {
	for h := lo; h <= hi; h++ {
		q.push(h)
	}
}

// drain removes and returns every queued height, oldest first.
func (q *resampleQueue) drain() []uint64 {
	out := q.heights
	q.heights = nil
	q.seen = make(map[uint64]bool)
	return out
}

// nodeState is one execution client's poll-to-poll bookkeeping.
type nodeState struct {
	name string
	rpc  migmon.Client

	timeline *migmon.Timeline
	reorg    *migmon.ReorgMemory
	nullTr   *migmon.NullTracker

	haveHead   bool
	lastHead   uint64
	lastBelowT uint64 // highest head this node was confirmed to have timestamp < T

	haveProgress bool
	lastProgress migmon.MigrationProgress

	// introspection is false for a client with no migration surface.
	introspection bool

	down bool // rpc reachability, deduped to one warn per outage

	// prevDone/doneFinalFired: a node's "done" is only trustworthy once
	// its fork block finalizes; fires after done holds for two consecutive
	// polls with no finality (tolerates the ~2s crossing/finalize race).
	prevDone       bool
	doneFinalFired bool

	// lastReorgAncestor anchors a final resample burst at this node's most
	// recent fork-block reorg.
	lastReorgAncestor uint64
	haveReorgAncestor bool
}

func newNodeState(name, url string, binaryTrieTime uint64) *nodeState {
	client := migmon.NewClient(name, url)
	return &nodeState{
		name:          name,
		rpc:           client,
		introspection: migmon.HasIntrospection(client),
		timeline:      migmon.NewTimeline(name, binaryTrieTime),
		reorg:         migmon.NewReorgMemory(name),
		nullTr:        migmon.NewNullTracker(name),
	}
}

// singleImplementation reports whether every state resolves to the same
// registry entry: N copies of one client proves determinism, not
// cross-implementation agreement.
func singleImplementation(states []*nodeState) bool {
	if len(states) == 0 {
		return false
	}
	first, ok := migmon.SpecFor(states[0].name)
	if !ok {
		return false
	}
	for _, ns := range states[1:] {
		spec, ok := migmon.SpecFor(ns.name)
		if !ok || spec != first {
			return false
		}
	}
	return true
}

func (ns *nodeState) warnDown(log *migmon.Log, method string, err error) {
	if ns.down {
		return
	}
	ns.down = true
	log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: fmt.Sprintf("%s unreachable: %v", method, err)})
}

func (ns *nodeState) clearDown(log *migmon.Log) {
	if !ns.down {
		return
	}
	ns.down = false
	log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: "rpc recovered"})
}

// pollOnce runs one node's poll tick: progress, head, the I* probe, and the
// timeline's findings.
func pollOnce(ctx context.Context, log *migmon.Log, binaryTrieTime uint64, ns *nodeState, quorum *migmon.IStarQuorum, q *resampleQueue) {
	var prog migmon.MigrationProgress
	if ns.introspection {
		raw, err := ns.rpc.Progress(ctx)
		if err != nil {
			ns.warnDown(log, "migration progress", err)
			return
		}
		prog, err = migmon.DecodeProgress(raw)
		if err != nil {
			ns.warnDown(log, "migration progress decode", err)
			return
		}
		log.Emit(migmon.Event{Kind: migmon.EvProgress, Node: ns.name, Phase: prog.Phase, Raw: raw})
		ns.lastProgress, ns.haveProgress = prog, true
	}
	ns.clearDown(log)

	head, err := ns.rpc.HeadNumber(ctx)
	if err != nil {
		ns.warnDown(log, "head number", err)
		return
	}
	log.Emit(migmon.Event{Kind: migmon.EvHead, Node: ns.name, Number: head})
	ns.lastHead, ns.haveHead = head, true

	// A recorded fork block is provisional until it finalizes.
	checkForkBlock(ctx, log, ns, quorum, q)

	if ns.timeline.IStarRecord() == nil {
		istar, crossed, err := findIStar(ctx, ns.rpc, &ns.lastBelowT, head, binaryTrieTime)
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: fmt.Sprintf("fork-block probe: %v", err)})
		} else if crossed {
			for _, e := range ns.timeline.ObserveIStar(*istar) {
				log.Emit(e)
			}
			for _, e := range quorum.Observe(ns.name, *istar, time.Now()) {
				log.Emit(e)
			}
		}
	}

	done := false
	if ns.introspection {
		done = prog.Phase == migmon.PhaseDone
		for _, e := range ns.timeline.ObservePoll(prog, head) {
			log.Emit(e)
		}
	}

	// Requires done on two consecutive polls before finalization to
	// tolerate the ~2s crossing/finalize race; fires once.
	if done && ns.prevDone && !ns.timeline.IStarIsFinal() && !ns.doneFinalFired {
		ns.doneFinalFired = true
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Node: ns.name, Finding: migmon.FindingBoundary,
			Detail: "reported done before its fork block finalized",
		})
	}
	ns.prevDone = done
}

// checkForkBlock drops the fork-block record if a reorg orphaned it, and
// marks it settled once the node reports the height finalized.
func checkForkBlock(ctx context.Context, log *migmon.Log, ns *nodeState, quorum *migmon.IStarQuorum, q *resampleQueue) {
	rec := ns.timeline.IStarRecord()
	if rec == nil || ns.timeline.IStarIsFinal() {
		return
	}
	hdr, err := ns.rpc.HeaderByNumber(ctx, rec.Number)
	if err != nil || hdr == nil {
		return // transient: the next tick tries again
	}
	if hdr.Hash != rec.Hash {
		for _, e := range ns.timeline.IStarReorged(hdr.Hash) {
			log.Emit(e)
			// Resample around the orphaned fork block now, cross-node.
			lo := uint64(1)
			if e.Number > 2 {
				lo = e.Number - 2
			}
			q.pushRange(lo, e.Number+2)
			ns.lastReorgAncestor, ns.haveReorgAncestor = e.Number, true
		}
		// Re-probe from scratch: the branch changed.
		ns.lastBelowT = 0
		return
	}
	fin, err := ns.rpc.HeaderByTag(ctx, "finalized")
	if err != nil || fin == nil || fin.Number < rec.Number {
		return
	}
	for _, e := range ns.timeline.IStarFinalized() {
		log.Emit(e)
	}
	// Burst-sample from the last divergence point up to I* itself.
	ancestor := ns.lastReorgAncestor
	if !ns.haveReorgAncestor {
		ancestor = uint64(1)
		if rec.Number > forkBurstWindow {
			ancestor = rec.Number - forkBurstWindow
		}
	}
	q.pushRange(ancestor, rec.Number)
	for _, e := range quorum.Finalize(ns.name, *rec) {
		log.Emit(e)
	}
}

// findIStar binary-searches for the first header with timestamp >= T,
// starting from lastBelowT since the last poll.
func findIStar(ctx context.Context, n migmon.Client, lastBelowT *uint64, head uint64, t uint64) (istar *migmon.IStar, crossed bool, err error) {
	hdr, err := n.HeaderByNumber(ctx, head)
	if err != nil {
		return nil, false, err
	}
	if hdr == nil {
		return nil, false, fmt.Errorf("head %d has no header", head)
	}
	if hdr.Time < t {
		*lastBelowT = head
		return nil, false, nil
	}

	lo, hi := *lastBelowT, head
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		midHdr, err := n.HeaderByNumber(ctx, mid)
		if err != nil {
			return nil, false, err
		}
		if midHdr == nil {
			return nil, false, fmt.Errorf("block %d has no header", mid)
		}
		if midHdr.Time >= t {
			hi = mid
		} else {
			lo = mid
		}
	}

	boundary := hdr
	if hi != head {
		if boundary, err = n.HeaderByNumber(ctx, hi); err != nil {
			return nil, false, err
		}
		if boundary == nil {
			return nil, false, fmt.Errorf("block %d has no header", hi)
		}
	}
	var parentTime uint64
	if hi > 0 {
		parentHdr, err := n.HeaderByNumber(ctx, hi-1)
		if err != nil {
			return nil, false, err
		}
		if parentHdr == nil {
			return nil, false, fmt.Errorf("block %d has no header", hi-1)
		}
		parentTime = parentHdr.Time
	}

	return &migmon.IStar{
		Number:     hi,
		Hash:       boundary.Hash,
		Time:       boundary.Time,
		ParentTime: parentTime,
	}, true, nil
}

// splitGrace: shared no-convergence/straddle-recovery grace bound.
const splitGrace = migmon.SplitGrace

// splitWatch times a cross-node canonical-chain disagreement.
type splitWatch struct {
	since time.Time
	fired bool
}

// observe reports the finding a persistent split earns, if any.
func (w *splitWatch) observe(now time.Time, split bool, height uint64, detail string) *migmon.Event {
	if !split {
		w.since, w.fired = time.Time{}, false
		return nil
	}
	if w.since.IsZero() {
		w.since = now
	}
	if w.fired || now.Sub(w.since) <= splitGrace {
		return nil
	}
	w.fired = true
	return &migmon.Event{
		Kind: migmon.EvCritical, Finding: migmon.FindingNoConvergence, Number: height,
		Detail: fmt.Sprintf("nodes have disagreed on the canonical chain for %.0fs, longer than any partition holds: %s",
			now.Sub(w.since).Seconds(), detail),
	}
}

// sampleOnce drains queued heights first (reorg/fork-burst events), then
// draws one random-depth sample behind the shallowest head.
func sampleOnce(ctx context.Context, log *migmon.Log, states []*nodeState, split *splitWatch, snap *snapshot, q *resampleQueue) {
	for _, h := range q.drain() {
		sampleAt(ctx, log, states, split, snap, q, h)
	}

	var minHead uint64
	haveHead := false
	for _, ns := range states {
		if ns.haveHead && (!haveHead || ns.lastHead < minHead) {
			minHead, haveHead = ns.lastHead, true
		}
	}
	if !haveHead {
		return
	}
	depth := uint64(migmon.SampleDepthMin + mrand.IntN(migmon.SampleDepthMax-migmon.SampleDepthMin+1))
	if depth > minHead {
		return // nothing this deep in the chain yet
	}
	sampleAt(ctx, log, states, split, snap, q, minHead-depth)
}

// sampleAt fetches every node's canonical hash and shadow root at height
// and runs root-mismatch/reorg/null-persistence over the results.
func sampleAt(ctx context.Context, log *migmon.Log, states []*nodeState, split *splitWatch, snap *snapshot, q *resampleQueue, height uint64) {
	samples := make([]migmon.NodeSample, 0, len(states))
	roots := make(map[string]string, len(states))
	for _, ns := range states {
		hdr, err := ns.rpc.HeaderByNumber(ctx, height)
		if err != nil || hdr == nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Number: height, Detail: fmt.Sprintf("sample header fetch: %v", err)})
			continue
		}
		root := ""
		if ns.introspection {
			if root, err = ns.rpc.ShadowRoot(ctx, hdr.Hash); err != nil {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Number: height, Detail: fmt.Sprintf("shadow root: %v", err)})
				continue
			}
		}
		hash := hdr.Hash
		samples = append(samples, migmon.NodeSample{Node: ns.name, Hash: hash, Root: root})
		roots[ns.name] = root

		for _, e := range ns.reorg.Observe(height, hash, ns.lastHead) {
			log.Emit(e)
			q.push(e.Number)
		}
		for _, h := range ns.reorg.Recent(reorgRecheckWindow) {
			if h == height {
				continue
			}
			rHdr, err := ns.rpc.HeaderByNumber(ctx, h)
			if err != nil || rHdr == nil {
				continue // a transient miss here just waits for the next tick
			}
			for _, e := range ns.reorg.Observe(h, rHdr.Hash, ns.lastHead) {
				log.Emit(e)
				q.push(e.Number)
			}
		}

		active := ns.introspection && ns.haveProgress &&
			(migmon.Active(ns.lastProgress.Binary) || migmon.Active(ns.lastProgress.Merkle))
		for _, e := range ns.nullTr.Observe(time.Now(), active, root == "") {
			log.Emit(e)
		}
	}
	if len(samples) == 0 {
		return
	}

	canonical := ""
	for _, s := range samples {
		if s.Hash != "" {
			canonical = s.Hash
			break
		}
	}
	log.Emit(migmon.Event{Kind: migmon.EvSample, Number: height, Hash: canonical, Roots: roots})

	postIStar := false
	for _, ns := range states {
		if b := ns.timeline.IStarRecord(); b != nil && height >= b.Number {
			postIStar = true
			break
		}
	}
	for _, e := range migmon.EvaluateSample(samples, postIStar) {
		log.Emit(e)
	}

	// A split during a partition is legal; outliving one is not.
	seen := map[string]string{}
	for _, s := range samples {
		if s.Hash != "" {
			seen[s.Hash] = s.Node
		}
	}
	detail := ""
	for hash, node := range seen {
		if detail != "" {
			detail += " vs "
		}
		detail += node + "=" + hash
	}
	isSplit := len(seen) > 1
	if e := split.observe(time.Now(), isSplit, height, detail); e != nil {
		log.Emit(*e)
	}
	snap.addSample(time.Now(), height, states, samples, isSplit)
}
