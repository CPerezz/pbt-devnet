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

// nodeState is one execution client's poll-to-poll bookkeeping. All findings
// logic lives in internal/migmon; this struct only remembers what a node
// needs carried from one tick to the next.
type nodeState struct {
	name string
	rpc  *node

	timeline *migmon.Timeline
	reorg    *migmon.ReorgMemory
	nullTr   *migmon.NullTracker

	haveHead   bool
	lastHead   uint64
	lastBelowT uint64 // highest head this node was confirmed to have timestamp < T

	haveProgress bool
	lastProgress migmon.MigrationProgress

	down bool // rpc reachability, deduped so an outage logs one warn, not one per poll
}

func newNodeState(name, url string, binaryTrieTime uint64) *nodeState {
	return &nodeState{
		name:     name,
		rpc:      newNode(name, url),
		timeline: migmon.NewTimeline(name, binaryTrieTime),
		reorg:    migmon.NewReorgMemory(name),
		nullTr:   migmon.NewNullTracker(name),
	}
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
func pollOnce(ctx context.Context, log *migmon.Log, binaryTrieTime uint64, ns *nodeState, quorum *migmon.BStarQuorum) {
	raw, err := getMigrationProgress(ctx, ns.rpc)
	if err != nil {
		ns.warnDown(log, "debug_migrationProgress", err)
		return
	}
	prog, err := migmon.DecodeProgress(raw)
	if err != nil {
		ns.warnDown(log, "debug_migrationProgress decode", err)
		return
	}
	ns.clearDown(log)
	log.Emit(migmon.Event{Kind: migmon.EvProgress, Node: ns.name, Phase: prog.Phase, Raw: raw})
	ns.lastProgress, ns.haveProgress = prog, true

	head, err := getBlockNumber(ctx, ns.rpc)
	if err != nil {
		ns.warnDown(log, "eth_blockNumber", err)
		return
	}
	log.Emit(migmon.Event{Kind: migmon.EvHead, Node: ns.name, Number: head})
	ns.lastHead, ns.haveHead = head, true

	if ns.timeline.BStarRecord() == nil {
		bstar, crossed, err := findBStar(ctx, ns.rpc, &ns.lastBelowT, head, binaryTrieTime)
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Detail: fmt.Sprintf("bstar probe: %v", err)})
		} else if crossed {
			for _, e := range ns.timeline.ObserveBStar(*bstar) {
				log.Emit(e)
			}
			for _, e := range quorum.Add(ns.name, *bstar) {
				log.Emit(e)
			}
		}
	}

	for _, e := range ns.timeline.ObservePoll(prog, head) {
		log.Emit(e)
	}
}

// findBStar looks for the first header with timestamp >= T. It assumes
// genesis's timestamp is < T (T is a future activation time) and that lo,
// the highest head this node was last confirmed to be below T at, still
// holds — so it only needs to binary-search the gap since the last poll,
// not the whole chain.
func findBStar(ctx context.Context, n *node, lastBelowT *uint64, head uint64, t uint64) (bstar *migmon.BStar, crossed bool, err error) {
	hdr, err := getHeader(ctx, n, head)
	if err != nil {
		return nil, false, err
	}
	if uint64(hdr.Timestamp) < t {
		*lastBelowT = head
		return nil, false, nil
	}

	lo, hi := *lastBelowT, head
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		midHdr, err := getHeader(ctx, n, mid)
		if err != nil {
			return nil, false, err
		}
		if uint64(midHdr.Timestamp) >= t {
			hi = mid
		} else {
			lo = mid
		}
	}

	boundary := hdr
	if hi != head {
		if boundary, err = getHeader(ctx, n, hi); err != nil {
			return nil, false, err
		}
	}
	var parentTime uint64
	if hi > 0 {
		parentHdr, err := getHeader(ctx, n, hi-1)
		if err != nil {
			return nil, false, err
		}
		parentTime = uint64(parentHdr.Timestamp)
	}

	return &migmon.BStar{
		Number:     hi,
		Hash:       boundary.Hash.Hex(),
		Time:       uint64(boundary.Timestamp),
		ParentTime: parentTime,
	}, true, nil
}

// sampleOnce runs one cross-node shadow-root sample: pick a random depth
// behind the shallowest head, fetch every node's canonical hash and shadow
// root there, and run F1/reorg/null-persistence over the results.
func sampleOnce(ctx context.Context, log *migmon.Log, states []*nodeState) {
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
	height := minHead - depth

	samples := make([]migmon.NodeSample, 0, len(states))
	roots := make(map[string]string, len(states))
	for _, ns := range states {
		hdr, err := getHeader(ctx, ns.rpc, height)
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Number: height, Detail: fmt.Sprintf("sample header fetch: %v", err)})
			continue
		}
		root, err := getShadowStateRoot(ctx, ns.rpc, hdr.Hash)
		if err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: ns.name, Number: height, Detail: fmt.Sprintf("shadowStateRoot: %v", err)})
			continue
		}
		hash := hdr.Hash.Hex()
		samples = append(samples, migmon.NodeSample{Node: ns.name, Hash: hash, Root: root})
		roots[ns.name] = root

		for _, e := range ns.reorg.Observe(height, hash, ns.lastHead) {
			log.Emit(e)
		}
		for _, h := range ns.reorg.Recent(reorgRecheckWindow) {
			if h == height {
				continue
			}
			rHdr, err := getHeader(ctx, ns.rpc, h)
			if err != nil {
				continue // a transient miss here just waits for the next tick
			}
			for _, e := range ns.reorg.Observe(h, rHdr.Hash.Hex(), ns.lastHead) {
				log.Emit(e)
			}
		}

		active := ns.haveProgress && (migmon.Active(ns.lastProgress.Binary) || migmon.Active(ns.lastProgress.Merkle))
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
}
