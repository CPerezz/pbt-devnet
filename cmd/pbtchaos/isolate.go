package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// isolationLoop schedules a reorg every minBlocks..maxBlocks. It never runs one while
// anything else is in flight: two overlapping disruptions produce a mess that proves
// nothing about either.
func (c *chaos) isolationLoop(ctx context.Context) {
	for {
		gap := c.cfg.isolateMin
		if c.cfg.isolateMax > c.cfg.isolateMin {
			gap += uint64(rand.Int63n(int64(c.cfg.isolateMax - c.cfg.isolateMin + 1)))
		}
		if err := waitBlocks(ctx, c.els, gap); err != nil {
			return // context cancelled
		}
		if c.quiesced.Load() {
			return // quiesced is terminal: no more forks will ever be scheduled
		}
		if c.busy() {
			continue
		}
		if err := c.submit(job{name: "proposer-fork", run: c.isolationFork}); err != nil {
			c.log.Warn("could not queue proposer fork", "err", err)
		}
	}
}

// isolationWindow returns how long the next isolation fork should hold the proposer, after
// applying --max-depth. The window always needs at least one lead slot plus the proposal
// slot itself -- that floor is depth 1, the single duty this command has always targeted --
// so depth here counts the slots of headroom beyond that floor.
//
// requestedDepth is what the configured --isolate-for would have produced; depth is what
// --max-depth allows. They differ only when the window had to be shortened, which is what
// clamped reports.
func (c *chaos) isolationWindow() (window time.Duration, depth, requestedDepth uint64, clamped bool) {
	slots := uint64(1)
	if c.cfg.slotSeconds > 0 {
		if s := uint64(c.cfg.isolateFor / c.cfg.slotSeconds); s > 0 {
			slots = s
		}
	}
	requestedDepth = slots - 1
	depth, clamped = clampDepth(requestedDepth, c.cfg.maxDepth)
	if !clamped {
		return c.cfg.isolateFor, depth, requestedDepth, false
	}
	return time.Duration(depth+1) * c.cfg.slotSeconds, depth, requestedDepth, true
}

// isolationFork cuts the p2p of the node that is about to propose.
//
// A partition, not shaping. disruptoor only accepts scope ["include_control"] for shaping,
// which slows the engine API too, so the proposer misses its slot entirely -- and a missed
// slot reorgs nothing. A p2p partition leaves the engine API alone: the proposer builds
// normally and only PUBLICATION is cut, so it takes its own block as head while everyone
// else builds on the parent, and unwinds when the partition clears. The reorg therefore
// lands on the isolated node, which is where it must be observed.
func (c *chaos) isolationFork(ctx context.Context) result {
	window, depth, requestedDepth, depthClamped := c.isolationWindow()
	res := result{Name: "proposer-fork", Started: time.Now().UTC().Format(time.RFC3339), Depth: depth}
	if depthClamped {
		res.RequestedDepth = requestedDepth
		c.log.Warn("clamping periodic isolation window", "requested_depth", requestedDepth,
			"max_depth", c.cfg.maxDepth, "applied_depth", depth)
	}

	node, slot, err := c.nextProposer(ctx)
	if err != nil {
		res.Outcome = "skipped"
		res.Detail = err.Error()
		return res
	}
	if c.cfg.protected[node] {
		res.Outcome = "skipped"
		res.Detail = fmt.Sprintf("node %d proposes next but is protected from disruption", node)
		return res
	}

	others := make([]int, 0, len(c.els))
	for i := range c.els {
		if i+1 != node {
			others = append(others, i+1)
		}
	}
	if len(others) == 0 {
		res.Outcome = "skipped"
		res.Detail = "only one node, so nothing to be isolated from"
		return res
	}

	// The duty is a few slots out, so hold off until it is imminent. Isolating as soon
	// as the duty is known would cut the node during slots it is not proposing in and
	// let it publish normally in the one that matters.
	if err := c.waitUntilSlot(ctx, slot-1); err != nil {
		res.Outcome = "skipped"
		res.Detail = fmt.Sprintf("waiting for slot %d: %v", slot-1, err)
		return res
	}

	// Watching starts before the isolation, so the block that gets orphaned is already
	// recorded as canonical when it disappears.
	watch := c.watchReorg(ctx, 10*c.cfg.slotSeconds)

	if err := c.d.Partition(fmt.Sprintf("proposer-%d", slot), others, []int{node}); err != nil {
		res.Outcome = "error"
		res.Detail = fmt.Sprintf("could not isolate node %d: %v", node, err)
		return res
	}
	c.log.Info("isolating proposer", "node", node, "slot", slot, "for", window)

	sleep(ctx, window)
	if err := c.d.Clear(); err != nil {
		c.log.Error("could not clear the isolation", "err", err)
	}

	ev := <-watch
	if ev.detected {
		res.Reorged = true
		res.Outcome = "reorged"
		c.countReorg(ev.client)
		res.Detail = fmt.Sprintf("isolated node %d at slot %d; %s saw block %d change %s -> %s",
			node, slot, ev.client, ev.height, short(ev.before), short(ev.after))
		c.log.Info("reorg observed", "client", ev.client, "height", ev.height,
			"before", short(ev.before), "after", short(ev.after), "node", node, "slot", slot)
	} else {
		// Not a failure. The isolated node may not have been due to propose after all,
		// and lighthouse declines to re-org at epoch boundaries.
		res.Outcome = "no-reorg"
		res.Detail = fmt.Sprintf("node %d isolated at slot %d, no client changed a block hash", node, slot)
		c.log.Info("no reorg from this attempt", "node", node, "slot", slot)
	}
	return res
}

// participantFor maps a validator index to its 1-based participant number, or 0 if the
// index belongs to nobody known.
//
// With --validator-counts unset, ethereum-package hands out sequential validator ranges of
// one uniform size, one range per participant, so the index divided by that size IS the
// participant -- the formula this command has always used. With it set, stake is not shared
// out evenly and the mapping is a prefix-sum lookup instead: participant i owns the
// half-open range [sum(counts[0..i-1]), sum(counts[0..i])).
func (c *chaos) participantFor(validatorIndex uint64) int {
	if len(c.cfg.validatorCounts) == 0 {
		return int(validatorIndex/c.cfg.validatorsPer) + 1
	}
	var sum uint64
	for i, n := range c.cfg.validatorCounts {
		if validatorIndex < sum+n {
			return i + 1
		}
		sum += n
	}
	return 0
}

// nextProposer returns the participant number that proposes a few slots from now, and
// that slot. A few slots of lead time is needed because the isolation has to be in place
// before the proposer starts building.
func (c *chaos) nextProposer(ctx context.Context) (int, uint64, error) {
	b := c.cls[0]
	head, err := b.headSlot(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("head slot: %w", err)
	}
	const slotsPerEpoch = 32
	epoch := head / slotsPerEpoch

	// Look at this epoch and the next, so a head near the end of an epoch still finds a
	// target rather than skipping the attempt.
	var duties []duty
	for _, e := range []uint64{epoch, epoch + 1} {
		d, err := b.duties(ctx, e)
		if err != nil {
			continue
		}
		duties = append(duties, d...)
	}
	if len(duties) == 0 {
		return 0, 0, fmt.Errorf("no proposer duties available")
	}

	for _, d := range duties {
		if d.Slot < head+2 {
			continue // too soon to get the isolation in place
		}
		node := c.participantFor(d.ValidatorIndex)
		if node < 1 || node > c.cfg.nodeCount {
			continue
		}
		return node, d.Slot, nil
	}
	return 0, 0, fmt.Errorf("no upcoming proposer maps to a known node")
}

// waitUntilSlot blocks until the consensus layer reports it has reached target.
func (c *chaos) waitUntilSlot(ctx context.Context, target uint64) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(time.Duration(64) * c.cfg.slotSeconds)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("slot %d never arrived", target)
			}
			cur, err := c.cls[0].headSlot(ctx)
			if err != nil {
				continue
			}
			if cur >= target {
				return nil
			}
		}
	}
}

type reorgEvent struct {
	detected bool
	client   string
	height   uint64
	before   common.Hash
	after    common.Hash
}

// watchReorg polls EVERY client and reports the first height whose canonical hash
// changes on any of them. A changed hash at an unchanged height is the definition of a
// reorg, and it needs no cooperation from the clients' logs.
//
// All clients, not one: the node that has to unwind is usually the disrupted one, and
// watching only its undisturbed peers would miss exactly the case being produced.
func (c *chaos) watchReorg(ctx context.Context, window time.Duration) <-chan reorgEvent {
	out := make(chan reorgEvent, 1)
	go func() {
		defer close(out)
		seen := map[string]map[uint64]common.Hash{}
		for _, e := range c.els {
			seen[e.name] = map[uint64]common.Hash{}
		}
		deadline := time.After(window)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				out <- reorgEvent{}
				return
			case <-deadline:
				out <- reorgEvent{}
				return
			case <-tick.C:
				for _, e := range c.els {
					n, _, err := e.head(ctx)
					if err != nil {
						continue // a partitioned node may refuse; that is expected here
					}
					// Re-read a short trailing window rather than only the head: the
					// orphaned block is usually a block or two back by the time the
					// replacement lands.
					from := uint64(0)
					if n > 4 {
						from = n - 4
					}
					for h := from; h <= n; h++ {
						got := e.hashAt(ctx, h)
						if got == (common.Hash{}) {
							continue
						}
						was, ok := seen[e.name][h]
						if ok && was != got {
							out <- reorgEvent{detected: true, client: e.name, height: h, before: was, after: got}
							return
						}
						seen[e.name][h] = got
					}
				}
			}
		}
	}()
	return out
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func short(h common.Hash) string {
	s := h.Hex()
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
