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
		if c.busy() {
			continue
		}
		if err := c.submit(job{name: "proposer-fork", run: c.isolationFork}); err != nil {
			c.log.Warn("could not queue proposer fork", "err", err)
		}
	}
}

// isolationFork cuts the p2p of the node that is about to propose.
//
// Egress DELAY was the obvious mechanism and it does not work: disruptoor v0 only
// accepts scope ["include_control"] for shaping, which slows the engine API too, so the
// proposer cannot assemble a payload before its deadline and simply skips the slot. A
// missed slot is a non-event -- the next proposer builds on the same parent and nothing
// was ever reorged.
//
// A partition is scoped to p2p and leaves the engine API alone, so the proposer builds
// its block normally and only its PUBLICATION is cut. Its own execution client accepts
// that block as head; everyone else sees an empty slot and builds on the parent. When
// the partition clears, the proposer meets a heavier chain that does not contain its
// block and has to unwind it -- which is the reorg, and it lands on the node that has
// the doomed block, so that is where it must be observed.
func (c *chaos) isolationFork(ctx context.Context) result {
	res := result{Name: "proposer-fork", Started: time.Now().UTC().Format(time.RFC3339), Depth: 1}

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
	c.log.Info("isolating proposer", "node", node, "slot", slot, "for", c.cfg.isolateFor)

	sleep(ctx, c.cfg.isolateFor)
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
		// ethereum-package hands out sequential validator ranges, one block per
		// participant, so the index divided by the range size IS the participant.
		node := int(d.ValidatorIndex/c.cfg.validatorsPer) + 1
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
