package main

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// majorityTip returns a block the majority agrees on and that is far enough back to be
// settled, which is the branch the heal is expected to keep.
//
// The margin matters. Sampling the actual tip picks a block that is seconds old and
// barely attested, and normal fork choice can still replace it when the minority rejoins
// -- which is ordinary tip churn, not the doomed branch winning.
func majorityTip(ctx context.Context, majority []*el) (uint64, common.Hash) {
	n, err := lowestHead(ctx, majority)
	if err != nil || n <= tipMargin {
		return 0, common.Hash{}
	}
	n -= tipMargin
	same, seen := agreed(ctx, majority, n)
	if !same {
		return 0, common.Hash{}
	}
	return n, seen[majority[0].name]
}

// tipMargin is how far below the majority's head the survival check anchors itself.
const tipMargin = 4

// orphanDepth counts the blocks on the minority's chain that the majority does not share.
func (c *chaos) orphanDepth(ctx context.Context, minority *el, majority []*el) int {
	n, _, err := minority.head(ctx)
	if err != nil {
		return 0
	}
	ref := majority[0]
	for depth := 0; depth < 256 && n > 0; depth, n = depth+1, n-1 {
		mine := minority.hashAt(ctx, n)
		theirs := ref.hashAt(ctx, n)
		if mine == (common.Hash{}) || theirs == (common.Hash{}) {
			continue
		}
		if mine == theirs {
			return depth
		}
	}
	return 0
}

// minChainHeight is how much chain a scenario wants behind it before it starts.
const minChainHeight = 24

// awaitDivergence waits until the minority and the majority are demonstrably on different
// chains, which is the premise every scenario rests on.
func (c *chaos) awaitDivergence(ctx context.Context, minority *el, majority []*el, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		// Compare at the MAJORITY's head, not at the lowest head across everyone.
		//
		// The minority falls behind the moment it is cut off, so the lowest common height
		// is its own head -- a block both sides still agree on. The split is real and
		// invisible there, which made this wait time out and report a scenario
		// inconclusive when the partition had in fact bitten. Asking whether the minority
		// has the majority's head answers the question directly.
		if n, err := lowestHead(ctx, majority); err == nil && n > 0 {
			want := majority[0].hashAt(ctx, n)
			if want != (common.Hash{}) && minority.hashAt(ctx, n) != want {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// healQuietly clears disruptoor without letting a clear failure mask the reason the
// scenario is being abandoned.
func (c *chaos) healQuietly(ctx context.Context) {
	if err := c.d.clear(); err != nil {
		c.log.Error("could not clear disruptoor state", "err", err)
	}
}

// checkGone asks the scenario's own predicate, on the surviving branch, whether the
// doomed change is still there -- and separately whether anything that was supposed to
// outlive the reorg did.
func (c *chaos) checkGone(ctx context.Context, sc *scenario, r *run) (string, error) {
	var still []string
	for _, e := range r.all() {
		on, err := sc.effect(ctx, r, e, r.at)
		if err != nil {
			return "", fmt.Errorf("%s: %w", e.name, err)
		}
		if on {
			still = append(still, e.name)
		}
	}
	if len(still) > 0 {
		sort.Strings(still)
		// Wrong is not the same as inconsistent. Every client giving the same wrong
		// answer is a harness or specification question; clients differing is two
		// implementations disagreeing, which is why this devnet exists.
		kind := "clients DISAGREE"
		if len(still) == len(r.all()) {
			kind = "every client gives the same wrong answer"
		}
		return "", fmt.Errorf("%s: the reorged-out change is still present at block %s on %s",
			kind, r.at, strings.Join(still, " "))
	}

	note := fmt.Sprintf("the doomed change is gone from every client at block %s", r.at)
	if sc.survives != nil {
		for _, e := range r.all() {
			if err := sc.survives(ctx, r, e, r.at); err != nil {
				return "", fmt.Errorf("state that should have outlived the reorg is missing: %w", err)
			}
		}
		note += "; the shared blob still reads back on every client"
	}
	return note, nil
}

// awaitHeight waits for every client to have SOME block at n. A client that has just
// rejoined is still catching up, and "has not got there yet" is not "disagrees".
func awaitHeight(ctx context.Context, els []*el, n uint64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		missing := false
		for _, e := range els {
			if e.hashAt(ctx, n) == (common.Hash{}) {
				missing = true
				break
			}
		}
		if !missing {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
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
	// A scenario needs history behind it. Run one seconds after genesis and the anchor
	// lands on block 2, the fee market has not settled, and the partition competes with
	// nodes still finding each other -- all of which produce findings about the harness
	// rather than the clients.
	if h, err := lowestHead(ctx, c.els); err == nil && h < minChainHeight {
		if err := waitBlocks(ctx, c.els, minChainHeight-h); err != nil {
			res.Outcome = "skipped"
			res.Detail = fmt.Sprintf("waiting for the chain to reach block %d: %v", minChainHeight, err)
			return res
		}
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

	// Send only once the two sides really are on different chains. disruptoor applies
	// its rules asynchronously, so a transaction sent immediately after the partition
	// call can still reach the majority and be mined on the branch that SURVIVES -- and
	// then looks like state that refused to go away. code-sole reported exactly that
	// against all four clients, anchored at block 2.
	if !c.awaitDivergence(ctx, minority, majority, 90*time.Second) {
		c.healQuietly(ctx)
		res.Outcome = "inconclusive"
		res.Detail = "the partition never separated the chains, so nothing could be doomed"
		c.log.Warn("partition did not bite", "name", sc.name, "minority", minority.name)
		return res
	}

	applyErr := sc.apply(ctx, r)
	if applyErr != nil {
		// Heal at once. Holding the split for the full depth when the doomed state was
		// never written keeps the minority orphaned for minutes and verifies nothing --
		// it just looks like the devnet is broken.
		c.healQuietly(ctx)
		res.Outcome = "inconclusive"
		res.Detail = "the doomed transactions never landed: " + applyErr.Error()
		c.log.Error("scenario transactions failed on the minority; healed early",
			"name", sc.name, "err", applyErr)
		return res
	}

	// The experiment has to have been set up before it is worth running. The doomed
	// change must be visible on the minority -- otherwise a reverted transaction would
	// let the scenario "pass" on an absence that was always there -- and invisible on the
	// majority, which is what proves it is confined to the branch about to die.
	if on, err := sc.effect(ctx, r, minority, nil); err != nil || !on {
		c.healQuietly(ctx)
		res.Outcome = "inconclusive"
		res.Detail = fmt.Sprintf("the doomed change is not visible on the minority %s, so there is "+
			"nothing for the reorg to remove (%s)", minority.name, r.describeEffect(ctx, sc, c.els, nil))
		return res
	}
	if on, err := sc.effect(ctx, r, majority[0], nil); err != nil || on {
		c.healQuietly(ctx)
		res.Outcome = "inconclusive"
		res.Detail = fmt.Sprintf("the doomed change is already visible on the majority %s, so it "+
			"reached the surviving branch and was never doomed (%s)",
			majority[0].name, r.describeEffect(ctx, sc, c.els, nil))
		return res
	}
	c.log.Info("doomed state confirmed on the minority alone", "name", sc.name)

	// Measure depth on the majority: the minority builds slowly while partitioned, so
	// waiting on the slowest client would stretch a depth-10 scenario forever.
	if err := waitBlocks(ctx, majority, depth); err != nil {
		c.log.Warn("did not reach the requested depth", "err", err)
	}

	// How much chain is about to be thrown away. Walk back from the minority's head until
	// it agrees with the majority; the gap is the branch that dies. This is the number
	// that answers "are we only ever getting one-block forks?" without needing a UI, and
	// it should match geth's own `Chain reorg detected ... drop=N`.
	orphaned := c.orphanDepth(ctx, minority, majority)
	res.Orphaned = orphaned
	c.log.Info("branch about to be abandoned", "name", sc.name, "blocks", orphaned,
		"on", minority.name)

	// Record the branch that is MEANT to survive, before healing, so survival is checked
	// against a hash taken while the two branches still existed separately.
	wantHeight, wantHash := majorityTip(ctx, majority)

	if err := c.d.clear(); err != nil {
		res.Outcome = "error"
		res.Detail = "heal: " + err.Error()
		return res
	}
	c.log.Info("healed", "name", sc.name, "expect_height", wantHeight, "expect_hash", short(wantHash))

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

	if wantHash == (common.Hash{}) {
		// Without an anchor the only thing left to check against is head, where a
		// re-mined transaction will have restored the doomed state -- so the check would
		// be meaningless rather than merely weaker.
		res.Outcome = "inconclusive"
		res.Detail = "could not record a settled majority block before the heal, so there is " +
			"no branch-anchored height to verify against"
		return res
	}
	if !awaitHeight(ctx, c.els, wantHeight, 2*time.Minute) {
		res.Outcome = "inconclusive"
		res.Detail = fmt.Sprintf("not every client reached block %d after the heal", wantHeight)
		return res
	}

	// The intended branch has to be the one that won. If the doomed branch survived
	// instead, every check below would be inverted, so say so and stop.
	_, seen := agreed(ctx, c.els, wantHeight)
	var wrong []string
	for name, h := range seen {
		if h != wantHash {
			wrong = append(wrong, fmt.Sprintf("%s=%s", name, short(h)))
		}
	}
	if len(wrong) > 0 {
		sort.Strings(wrong)
		res.Outcome = "FINDING"
		res.Detail = fmt.Sprintf(
			"the majority branch did not survive: block %d was %s on the majority before the heal; after: %s",
			wantHeight, short(wantHash), strings.Join(wrong, " "))
		c.log.Error("wrong branch survived", "name", sc.name, "height", wantHeight, "want", short(wantHash))
		return res
	}
	res.Survivor = fmt.Sprintf("majority block %d %s", wantHeight, short(wantHash))
	r.at = new(big.Int).SetUint64(wantHeight)

	// The reorg happened, so the minority was made to unwind: count it, or a run of
	// scenarios reads as no reorg coverage at all.
	c.countReorg(minority.name)

	note, err := c.checkGone(ctx, sc, r)

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
