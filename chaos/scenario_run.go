package main

import (
	"context"
	"fmt"
	"time"
)

// runScenario splits the network, puts the scenario's state on the minority branch,
// holds it for depth blocks, heals, and then asks every client about that state.
//
// The heal is not assumed to work. A partition severs TCP sessions, and a consensus
// client that comes back with no peers can sit on its own branch indefinitely -- so if
// the clients have not reconverged the scenario reports "inconclusive" rather than a
// finding. A disagreement measured across a network that never healed says nothing
// about anyone's reorg handling.
func (c *chaos) runScenario(ctx context.Context, sc *scenario, depth uint64) result {
	res := result{Name: sc.name, Started: time.Now().UTC().Format(time.RFC3339), Depth: depth}

	if len(c.els) < 2 {
		res.Outcome = "skipped"
		res.Detail = "a scenario needs at least two execution clients"
		return res
	}
	if len(c.keys) == 0 {
		res.Outcome = "skipped"
		res.Detail = "no --key given, so nothing can be sent"
		return res
	}

	// The LAST participant is the minority. On the default devnet that is besu, which
	// makes the second implementation the one that has to unwind its own branch.
	minority := c.els[len(c.els)-1]
	majority := c.els[:len(c.els)-1]
	minorityNode := len(c.els)
	majorityNodes := make([]int, 0, len(majority))
	for i := range majority {
		majorityNodes = append(majorityNodes, i+1)
	}

	r := &run{c: c, minority: minority, majority: majority}

	var err error
	if r.viaMaj, err = newSender(ctx, c.keys[0], majority[0]); err != nil {
		res.Outcome = "error"
		res.Detail = err.Error()
		return res
	}
	// A second key, so the doomed transactions never share a nonce sequence with the
	// setup ones -- the two branches would otherwise fight over the same nonces.
	minKey := c.keys[0]
	if len(c.keys) > 1 {
		minKey = c.keys[1]
	}
	if r.viaMin, err = newSender(ctx, minKey, minority); err != nil {
		res.Outcome = "error"
		res.Detail = err.Error()
		return res
	}

	c.log.Info("scenario starting", "name", sc.name, "depth", depth,
		"minority", minority.name, "majority", len(majority))

	if sc.setup != nil {
		if err := sc.setup(ctx, r); err != nil {
			res.Outcome = "error"
			res.Detail = "setup: " + err.Error()
			return res
		}
		// Let the setup settle onto the chain before anything is cut, so it is
		// unambiguously part of the surviving branch.
		if err := waitBlocks(ctx, c.els, 2); err != nil {
			res.Outcome = "error"
			res.Detail = "waiting for setup to settle: " + err.Error()
			return res
		}
		c.log.Info("scenario setup on the surviving branch", "name", sc.name, "survivor", r.survivor.Hex())
	}

	if err := c.d.partition("pbt-"+sc.name, majorityNodes, []int{minorityNode}); err != nil {
		res.Outcome = "error"
		res.Detail = "partition: " + err.Error()
		return res
	}
	c.log.Info("partitioned", "majority", majorityNodes, "minority", minorityNode)

	applyErr := sc.apply(ctx, r)
	if applyErr != nil {
		// Heal at once. Holding the split for the full depth when the doomed state was
		// never written keeps the minority orphaned for minutes and verifies nothing --
		// it just looks like the devnet is broken.
		c.log.Error("scenario transactions failed on the minority; healing early",
			"name", sc.name, "err", applyErr)
	} else if err := waitBlocks(ctx, majority, depth); err != nil {
		// Measure depth on the majority: the minority builds slowly while partitioned,
		// so waiting on the slowest client would stretch a depth-10 scenario forever.
		c.log.Warn("did not reach the requested depth", "err", err)
	}

	if err := c.d.clear(); err != nil {
		res.Outcome = "error"
		res.Detail = "heal: " + err.Error()
		return res
	}
	c.log.Info("healed", "name", sc.name)

	if applyErr != nil {
		res.Outcome = "inconclusive"
		res.Detail = "the doomed transactions never landed: " + applyErr.Error()
		return res
	}

	height, ok := c.waitAgreement(ctx, 4*time.Minute)
	if !ok {
		n, _ := lowestHead(ctx, c.els)
		res.Outcome = "inconclusive"
		res.Detail = "clients did not reconverge within 4m of the heal, so any state difference " +
			"reflects an unhealed network rather than reorg handling; " + c.describeDisagreement(ctx, n)
		c.log.Warn("no reconvergence after heal", "name", sc.name)
		return res
	}
	res.Reorged = true
	c.log.Info("reconverged", "name", sc.name, "height", height)

	note, err := sc.verify(ctx, r)
	if err != nil {
		res.Outcome = "FINDING"
		res.Detail = err.Error()
		c.log.Error("clients disagree after the reorg", "name", sc.name, "detail", err)
		return res
	}
	res.Outcome = "ok"
	res.Detail = note
	c.log.Info("scenario passed", "name", sc.name, "detail", note)
	return res
}

// waitAgreement waits until every client reports the same hash at the highest height
// they all have, which is what "the reorg is over" looks like from outside.
func (c *chaos) waitAgreement(ctx context.Context, timeout time.Duration) (uint64, bool) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, false
		case <-tick.C:
			if time.Now().After(deadline) {
				return 0, false
			}
			n, err := lowestHead(ctx, c.els)
			if err != nil || n == 0 {
				continue
			}
			if same, _ := agreed(ctx, c.els, n); same {
				return n, true
			}
		}
	}
}

func (c *chaos) describeDisagreement(ctx context.Context, n uint64) string {
	_, seen := agreed(ctx, c.els, n)
	out := fmt.Sprintf("at block %d:", n)
	for name, h := range seen {
		out += fmt.Sprintf(" %s=%s", name, short(h))
	}
	return out
}
