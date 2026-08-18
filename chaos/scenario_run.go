package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// majorityTip returns the highest block every majority client agrees on, which is the
// branch the heal is expected to keep.
func majorityTip(ctx context.Context, majority []*el) (uint64, common.Hash) {
	n, err := lowestHead(ctx, majority)
	if err != nil || n == 0 {
		return 0, common.Hash{}
	}
	same, seen := agreed(ctx, majority, n)
	if !same {
		return 0, common.Hash{}
	}
	return n, seen[majority[0].name]
}

// runScenario splits the network, puts the scenario's state on the minority branch,
// holds it for depth blocks, heals, and then asks every client about that state.
//
// The heal is not assumed to work. A partition severs TCP sessions, and a consensus
// client that comes back with no peers can sit on its own branch indefinitely -- so if
// the clients have not reconverged the scenario reports "inconclusive" rather than a
// finding. A disagreement measured across a network that never healed says nothing
// about anyone's reorg handling.
func (c *chaos) runScenario(ctx context.Context, sc *scenario, depth uint64, minorityPick int) result {
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

	// Which node gets stranded rotates every run, so reorgs land on both client types
	// rather than always on whichever participant happens to be last. Pinning it to the
	// last participant is how besu came to look like it was orphaining a third of its
	// blocks: it was simply the minority every single time.
	//
	// With two of each client this also produces the most interesting case on its own --
	// one besu on the doomed branch while the other stays in the majority, so trie-log
	// rollback is measured against a client that never left the chain.
	idx := minorityPick
	if idx < 1 || idx > len(c.els) {
		idx = c.nextMinority()
	}
	minority := c.els[idx-1]
	minorityNode := idx
	majority := make([]*el, 0, len(c.els)-1)
	majorityNodes := make([]int, 0, len(c.els)-1)
	for i, e := range c.els {
		if i+1 != idx {
			majority = append(majority, e)
			majorityNodes = append(majorityNodes, i+1)
		}
	}

	r := &run{c: c, minority: minority, majority: majority}

	// Each run takes its own pair of keys. A transaction that gets reorged out stays
	// valid and re-enters the pool, so a key reused by the next scenario reads a nonce
	// that goes stale underneath it -- which showed up as scenarios passing and failing
	// in strict alternation.
	majKey, minKey := c.keysFor()
	var err error
	if r.viaMaj, err = newSender(ctx, majKey, majority[0]); err != nil {
		res.Outcome = "error"
		res.Detail = err.Error()
		return res
	}
	if r.viaMin, err = newSender(ctx, minKey, minority); err != nil {
		res.Outcome = "error"
		res.Detail = err.Error()
		return res
	}

	res.Minority = minority.name
	c.countMinority(minority.name)
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

	// Record the branch that is MEANT to survive, before healing, so survival is checked
	// against a hash taken while the two branches still existed separately. Without this
	// a scenario only ever checked state, and would read its assertions the wrong way
	// round if the minority's branch happened to win.
	wantHeight, wantHash := majorityTip(ctx, majority)

	if err := c.d.clear(); err != nil {
		res.Outcome = "error"
		res.Detail = "heal: " + err.Error()
		return res
	}
	c.log.Info("healed", "name", sc.name, "expect_height", wantHeight, "expect_hash", short(wantHash))

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

	// The intended branch has to be the one that won. If the doomed branch survived
	// instead, every state assertion below would be inverted, so say so and stop.
	if wantHash != (common.Hash{}) {
		if same, seen := agreed(ctx, c.els, wantHeight); !same || seen[majority[0].name] != wantHash {
			res.Outcome = "FINDING"
			res.Detail = fmt.Sprintf(
				"the majority branch did not survive: block %d was %s on the majority before the heal, %s after",
				wantHeight, short(wantHash), short(seen[majority[0].name]))
			c.log.Error("wrong branch survived", "name", sc.name, "height", wantHeight)
			return res
		}
		res.Survivor = fmt.Sprintf("majority block %d %s", wantHeight, short(wantHash))
	}

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
