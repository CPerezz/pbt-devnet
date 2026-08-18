package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// latencyLoop schedules a one-block reorg every latencyMin..latencyMax blocks. It never
// runs one while anything else is in flight: two overlapping disruptions produce a mess
// that proves nothing about either.
func (c *chaos) latencyLoop(ctx context.Context) {
	for {
		gap := c.cfg.latencyMin
		if c.cfg.latencyMax > c.cfg.latencyMin {
			gap += uint64(rand.Int63n(int64(c.cfg.latencyMax - c.cfg.latencyMin + 1)))
		}
		if err := waitBlocks(ctx, c.els, gap); err != nil {
			return // context cancelled
		}
		if c.busy() {
			continue
		}
		err := c.submit(job{name: "latency-fork", run: c.latencyFork})
		if err != nil {
			c.log.Warn("could not queue latency fork", "err", err)
		}
	}
}

// latencyFork delays the node that is about to propose. Its CL<->EL exchange misses the
// slot, the block lands late, and the next proposer builds over its parent instead --
// which is a one-block reorg for everyone who had already accepted the late block.
//
// Targeting the proposer rather than a random node is what makes these common enough to
// be worth running: on three nodes, random targeting lands one time in three.
func (c *chaos) latencyFork(ctx context.Context) result {
	res := result{Name: "latency-fork", Started: time.Now().UTC().Format(time.RFC3339)}

	node, slot, err := c.nextProposer(ctx)
	if err != nil {
		res.Outcome = "skipped"
		res.Detail = err.Error()
		return res
	}

	// Watching starts before the shaping does, so the block that gets orphaned is
	// already recorded as canonical when it disappears. Watch through a node that is
	// NOT the one being delayed: its own RPC answers late, which is enough to miss the
	// short window in which the reorg is visible.
	watch := c.watchReorg(ctx, c.observerExcept(node), 8*c.cfg.slotSeconds)

	name := fmt.Sprintf("late-proposer-%d", slot)
	if err := c.d.delay(name, []int{node}, c.cfg.latencyDelay); err != nil {
		res.Outcome = "error"
		res.Detail = fmt.Sprintf("could not shape node %d: %v", node, err)
		return res
	}
	c.log.Info("delaying proposer", "node", node, "slot", slot, "delay", c.cfg.latencyDelay)

	// Hold across the target slot, then let the network run unshaped so the competing
	// proposal can win.
	sleep(ctx, 2*c.cfg.slotSeconds)
	if err := c.d.clear(); err != nil {
		c.log.Error("could not clear shaping", "err", err)
	}

	ev := <-watch
	res.Depth = 1
	if ev.detected {
		res.Reorged = true
		res.Outcome = "reorged"
		res.Detail = fmt.Sprintf("node %d late at slot %d; block %d changed %s -> %s",
			node, slot, ev.height, short(ev.before), short(ev.after))
		c.log.Info("reorg observed", "height", ev.height,
			"before", short(ev.before), "after", short(ev.after), "node", node, "slot", slot)
	} else {
		// Not a failure. Lighthouse declines to re-org at epoch boundaries, and a
		// proposer that still makes its slot despite the delay simply produces no fork.
		res.Outcome = "no-reorg"
		res.Detail = fmt.Sprintf("node %d delayed at slot %d, chain did not fork", node, slot)
		c.log.Info("no reorg from this attempt", "node", node, "slot", slot)
	}
	return res
}

// nextProposer returns the participant number that proposes a few slots from now, and
// that slot. A few slots of lead time is needed because the shaping has to be in place
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
			continue // too soon to get shaping in place
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

type reorgEvent struct {
	detected bool
	height   uint64
	before   common.Hash
	after    common.Hash
}

// watchReorg polls one client and reports the first height whose canonical hash CHANGES.
// A changed hash at an unchanged height is the definition of a reorg, and it needs no
// cooperation from the client's logs.
// observerExcept picks a client to watch through, avoiding the one being disrupted.
func (c *chaos) observerExcept(node int) *el {
	for i, e := range c.els {
		if i+1 != node {
			return e
		}
	}
	return c.els[0]
}

func (c *chaos) watchReorg(ctx context.Context, e *el, window time.Duration) <-chan reorgEvent {
	out := make(chan reorgEvent, 1)
	go func() {
		defer close(out)
		seen := map[uint64]common.Hash{}
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
				n, _, err := e.head(ctx)
				if err != nil {
					continue
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
					was, ok := seen[h]
					if ok && was != got {
						out <- reorgEvent{detected: true, height: h, before: was, after: got}
						return
					}
					seen[h] = got
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
