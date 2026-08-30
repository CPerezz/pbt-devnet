package main

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// reorgRecheckWindow bounds how many previously-sampled heights get
// re-fetched each sample tick to catch a reorg under a height nothing else
// would touch again. Eight is a handful of extra header fetches per node per
// minute — cheap, and reorgs deep enough to escape it are not this devnet's
// failure mode; widen if a scenario needs a deeper window.
const reorgRecheckWindow = 8

// resampleQueueCap bounds the targeted-resample queue: reorg windows are a
// handful of heights per event and a finality burst is capped at
// forkBurstWindow, so 64 comfortably holds several concurrent events
// without growing memory over a long chaotic run.
const resampleQueueCap = 64

// forkBurstWindow is how far back of b* a bstar-final burst reaches when
// the node has no recorded reorg to anchor it - the same depth
// reorgRecheckWindow already treats as "recent" for reorg detection.
const forkBurstWindow = 8

// resampleQueue holds heights a reorg or fork-block event just made
// interesting for the next sample tick, FIFO, deduped, dropping the
// oldest entry once full. Reorged heights are exactly where rebuilt
// shadow state must be re-proven cross-node, and random sampling rarely
// lands there before the trail goes cold.
//
// The poll loop and the sample loop both push into the same queue instance
// from main's single select loop, never concurrently, so this needs no
// lock of its own.
type resampleQueue struct {
	heights []uint64
	seen    map[uint64]bool
}

func newResampleQueue() *resampleQueue {
	return &resampleQueue{seen: make(map[uint64]bool)}
}

// push enqueues height for immediate sampling. Heights below 1 (a clamped
// range's underflow) and repeats already queued are dropped.
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

// nodeState is one execution client's poll-to-poll bookkeeping. All findings
// logic lives in internal/migmon; this struct only remembers what a node
// needs carried from one tick to the next.
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

	// introspection is false for a client with no migration surface: it is
	// still watched over standard RPC, and its events say so rather than
	// letting silence read as agreement.
	introspection bool

	down bool // rpc reachability, deduped so an outage logs one warn, not one per poll

	// prevDone and doneFinalFired implement the live completion invariant:
	// a node's own "done" is only trustworthy once its fork block has
	// finalized. The critical fires only after done has held for two
	// consecutive polls with no finality yet, tolerating the ~2s race
	// between crossing the boundary and consensus marking it finalized.
	prevDone       bool
	doneFinalFired bool

	// lastReorgAncestor is the height of this node's most recent
	// fork-block reorg, if any - the anchor a bstar-final resample burst
	// starts from, since that is exactly where the settled record's
	// history last diverged.
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

// singleImplementation reports whether every state's node name resolves to
// the same registry entry: a run against N copies of one client image
// proves the chain is deterministic under chaos, not that independent
// implementations agree on the migration - the property cross-node checks
// exist to catch. Any node that matches no registry entry, or a set that
// splits across entries, is not "single-implementation" by this measure.
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

// pollOnce runs one node's poll tick: progress, head, the b* probe, and the
// timeline's findings.
func pollOnce(ctx context.Context, log *migmon.Log, binaryTrieTime uint64, ns *nodeState, quorum *migmon.BStarQuorum, q *resampleQueue) {
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

	// A recorded fork block is provisional until it finalizes: a reorg
	// spanning the activation can orphan it, and a node still judging the
	// boundary against a block nobody has would report a fault that no
	// longer exists.
	checkForkBlock(ctx, log, ns, quorum, q)

	if ns.timeline.BStarRecord() == nil {
		bstar, crossed, err := findBStar(ctx, ns.rpc, &ns.lastBelowT, head, binaryTrieTime)
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: fmt.Sprintf("fork-block probe: %v", err)})
		} else if crossed {
			for _, e := range ns.timeline.ObserveBStar(*bstar) {
				log.Emit(e)
			}
			for _, e := range quorum.Observe(ns.name, *bstar, time.Now()) {
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

	// F3 live completion: a node reporting done while its own fork block
	// has not finalized might just be the ~2s race between crossing the
	// boundary and consensus marking it finalized - tolerated by requiring
	// done on two consecutive polls before this fires, latched so a run
	// that stays done-but-not-final only reports it once.
	if done && ns.prevDone && !ns.timeline.BStarIsFinal() && !ns.doneFinalFired {
		ns.doneFinalFired = true
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Node: ns.name, Finding: migmon.FindingBoundary,
			Detail: "reported done before its fork block finalized",
		})
	}
	ns.prevDone = done
}

// checkForkBlock keeps one node's fork-block record honest: it drops the
// record when a reorg has orphaned it, and marks it settled once the node
// reports the height as finalized. Finality is read from the execution
// client's own "finalized" tag, which the consensus layer sets through the
// engine API - no second API to reach for.
func checkForkBlock(ctx context.Context, log *migmon.Log, ns *nodeState, quorum *migmon.BStarQuorum, q *resampleQueue) {
	rec := ns.timeline.BStarRecord()
	if rec == nil || ns.timeline.BStarIsFinal() {
		return
	}
	hdr, err := ns.rpc.HeaderByNumber(ctx, rec.Number)
	if err != nil || hdr == nil {
		return // transient: the next tick tries again
	}
	if hdr.Hash != rec.Hash {
		for _, e := range ns.timeline.BStarReorged(hdr.Hash) {
			log.Emit(e)
			// The orphaned fork block is exactly where rebuilt shadow
			// state must be re-proven cross-node; resample around it now
			// rather than waiting for random sampling to land there.
			lo := uint64(1)
			if e.Number > 2 {
				lo = e.Number - 2
			}
			q.pushRange(lo, e.Number+2)
			ns.lastReorgAncestor, ns.haveReorgAncestor = e.Number, true
		}
		// Re-probe from scratch: the whole branch changed, so the previous
		// "highest head below the activation" no longer bounds the search.
		ns.lastBelowT = 0
		return
	}
	fin, err := ns.rpc.HeaderByTag(ctx, "finalized")
	if err != nil || fin == nil || fin.Number < rec.Number {
		return
	}
	for _, e := range ns.timeline.BStarFinalized() {
		log.Emit(e)
	}
	// The fork block just settled: burst-sample from where this node's b*
	// record last diverged (or a fixed lookback if it never diverged) up
	// to b* itself, re-proving the rebuilt shadow state around the
	// boundary before random sampling gets around to it.
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

// findBStar looks for the first header with timestamp >= T. It assumes
// genesis's timestamp is < T (T is a future activation time) and that lo,
// the highest head this node was last confirmed to be below T at, still
// holds — so it only needs to binary-search the gap since the last poll,
// not the whole chain.
func findBStar(ctx context.Context, n migmon.Client, lastBelowT *uint64, head uint64, t uint64) (bstar *migmon.BStar, crossed bool, err error) {
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

	return &migmon.BStar{
		Number:     hi,
		Hash:       boundary.Hash,
		Time:       boundary.Time,
		ParentTime: parentTime,
	}, true, nil
}

// splitGrace is migmon's shared SplitGrace: the monitor fires F4 on it and
// the verifier waives straddle-recovery criticals against the same bound.
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

// sampleOnce runs one cross-node shadow-root sample tick: first it drains
// any heights a reorg or fork-block event just queued, sampling those
// immediately, then it draws one random-depth sample behind the shallowest
// head the way it always has. Queued heights go first because that is
// where rebuilt shadow state must be re-proven; random picks rarely land
// there before the trail goes cold.
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
// and runs F1/reorg/null-persistence over the results.
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

	postBStar := false
	for _, ns := range states {
		if b := ns.timeline.BStarRecord(); b != nil && height >= b.Number {
			postBStar = true
			break
		}
	}
	for _, e := range migmon.EvaluateSample(samples, postBStar) {
		log.Emit(e)
	}

	// Canonical-chain disagreement at one height is legal during a
	// partition; outliving one is not.
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
