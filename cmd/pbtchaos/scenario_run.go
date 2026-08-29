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
		// invisible there. Asking whether the minority
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
	if err := c.d.Clear(); err != nil {
		c.log.Error("could not clear disruptoor state", "err", err)
	}
}

// checkGone asks the scenario's own predicate, on the surviving branch, whether the
// doomed change is still there -- and separately whether anything that was supposed to
// outlive the reorg did.
func (c *chaos) checkGone(ctx context.Context, sc *scenario, r *run) (string, error) {
	var still []string
	for _, e := range r.all() {
		visible, total, err := sc.effect(ctx, r, e, r.at)
		if err != nil {
			return "", fmt.Errorf("%s: %w", e.name, err)
		}
		if visible > 0 {
			still = append(still, fmt.Sprintf("%s (%d/%d)", e.name, visible, total))
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
// laggards is what awaitHeight learned about the clients that never arrived.
type laggards struct {
	short  []string          // clients that never reached the target height
	frozen []string          // of those, the ones that also stopped advancing
	heads  map[string]uint64 // last head seen per client
}

// awaitHeight waits for every client to have block n, and reports who did not make it.
//
// Whether they were still moving separates "slow" from "wedged". A wedge surfaces HERE and not
// in waitAgreement: a node that stops on the canonical chain still agrees at the common height,
// so only the anchor it never reaches gives it away.
//
// The poll interval matches waitAgreement's because wedgeTicks counts observations, not
// seconds; polling faster would call ordinary propagation lag a wedge.
func awaitHeight(ctx context.Context, els []*el, n uint64, timeout time.Duration) (bool, laggards) {
	deadline := time.Now().Add(timeout)
	heads := newHeadTracker()
	var out laggards
	for {
		sample := map[string]uint64{}
		var short []string
		for _, e := range els {
			if h, _, err := e.head(ctx); err == nil {
				sample[e.name] = h
			}
			if e.hashAt(ctx, n) == (common.Hash{}) {
				short = append(short, e.name)
			}
		}
		heads.observe(sample)
		if len(short) == 0 {
			return true, laggards{}
		}
		sort.Strings(short)
		out = laggards{short: short, heads: sample}

		if time.Now().After(deadline) {
			out.frozen = both(heads.frozen(), short)
			return false, out
		}
		select {
		case <-ctx.Done():
			return false, out
		case <-time.After(5 * time.Second):
		}
	}
}

// describeHeads renders "name=height" for the named clients, so a report says how far behind
// they actually were rather than merely that they were.
func describeHeads(names []string, heads map[string]uint64) string {
	var parts []string
	for _, n := range names {
		if h, ok := heads[n]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", n, h))
		} else {
			parts = append(parts, n+"=?")
		}
	}
	return strings.Join(parts, " ")
}

// both returns the names present in each list, so a client is only called frozen if it is also
// one of the clients that failed to arrive.
func both(a, b []string) []string {
	in := map[string]bool{}
	for _, n := range b {
		in[n] = true
	}
	var out []string
	for _, n := range a {
		if in[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// runScenario splits the network, puts the scenario's state on the minority branch,
// holds it for depth blocks, heals, and then asks every client about that state.
//
// The heal is not assumed to work, and failing to reconverge after one is a claim about the
// clients: the partition is provably gone by then. Two readings are separated rather than
// merged. A client whose head stops advancing while the chain builds around it is wedged --
// besu's cross-fork roll failure looks exactly like that -- and is reported as a finding
// within about thirty seconds. A consensus client with no peers has nobody to agree with,
// which is the network, and stays "inconclusive".
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
	// rather than always on whichever participant happens to be last.
	//
	// With two of each client this also produces the most interesting case on its own --
	// one besu on the doomed branch while the other stays in the majority, so trie-log
	// rollback is measured against a client that never left the chain.
	idx := c.minorityFor(minorityPick)
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
	// that goes stale underneath it.
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

	if err := c.d.Partition("pbt-"+sc.name, majorityNodes, []int{minorityNode}); err != nil {
		res.Outcome = "error"
		res.Detail = "partition: " + err.Error()
		return res
	}
	c.log.Info("partitioned", "majority", majorityNodes, "minority", minorityNode)

	// Send only once the two sides really are on different chains. disruptoor applies
	// its rules asynchronously, so a transaction sent immediately after the partition
	// call can still reach the majority and be mined on the branch that SURVIVES -- and
	// then looks like state that refused to go away.
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
	// EVERY doomed write has to be visible here, not merely one of them: a scenario that wrote
	// half its state would otherwise verify the half that landed and silently skip the rest.
	if visible, total, err := sc.effect(ctx, r, minority, nil); err != nil || visible != total {
		c.healQuietly(ctx)
		res.Outcome = "inconclusive"
		res.Detail = fmt.Sprintf("the doomed change is not fully visible on the minority %s "+
			"(%d of %d), so there is nothing for the reorg to remove (%s)",
			minority.name, visible, total, r.describeEffect(ctx, sc, c.els, nil))
		return res
	}
	if visible, _, err := sc.effect(ctx, r, majority[0], nil); err != nil || visible != 0 {
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

	if err := c.d.Clear(); err != nil {
		res.Outcome = "error"
		res.Detail = "heal: " + err.Error()
		return res
	}
	c.log.Info("healed", "name", sc.name, "expect_height", wantHeight, "expect_hash", short(wantHash))

	height, ok, frozen := c.waitAgreement(ctx, 4*time.Minute)
	if !ok {
		// The partition is gone -- Clear() succeeded above -- so a chain that will not
		// reconverge is a claim about the clients, not about the network. Only one reading
		// still belongs to the network: a consensus client with no peers has nobody to agree
		// with, and that is what the peer counts are here to separate.
		n, _ := lowestHead(ctx, c.els)
		peers, byIndex := c.peerSummary(ctx)

		// A frozen head only means "wedged" if the client had any way to move. A consensus
		// client with no peers receives no blocks, so its execution client stands still by
		// starvation -- that is the network, and reporting it as a client defect would bury
		// the real thing under a finding per scenario.
		wedged, starved := classifyFrozen(frozen, byIndex)

		switch {
		case len(wedged) > 0:
			res.Outcome = "finding"
			res.Detail = fmt.Sprintf("%s stopped advancing while the rest of the chain kept "+
				"building, and the clients never reconverged after a heal that succeeded — %s. "+
				"It is not starved: see the peer counts. peers: %s; disruptoor: %s",
				strings.Join(wedged, ", "), c.describeDisagreement(ctx, n), peers,
				c.disruptoorSummary())
			c.log.Error("FINDING: client wedged after heal", "name", sc.name, "clients", wedged)
		case len(starved) > 0:
			res.Outcome = "inconclusive"
			res.Detail = fmt.Sprintf("%s stopped advancing, but its consensus client has no "+
				"peers, so it is starved rather than wedged — the network, not the client. "+
				"Restart it and re-run. peers: %s; %s", strings.Join(starved, ", "), peers,
				c.describeDisagreement(ctx, n))
			c.log.Warn("no reconvergence: client starved of peers", "name", sc.name, "clients", starved)
		default:
			res.Outcome = "finding"
			res.Detail = fmt.Sprintf("the clients did not reconverge within 4m of a heal that "+
				"succeeded, and every client kept building — %s. peers: %s; disruptoor: %s",
				c.describeDisagreement(ctx, n), peers, c.disruptoorSummary())
			c.log.Error("FINDING: no reconvergence after heal", "name", sc.name)
		}
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
	if ok, lag := awaitHeight(ctx, c.els, wantHeight, 2*time.Minute); !ok {
		peers, byIndex := c.peerSummary(ctx)
		wedged, starved := classifyFrozen(lag.frozen, byIndex)
		switch {
		case len(wedged) > 0:
			res.Outcome = "finding"
			res.Detail = fmt.Sprintf("%s never reached block %d after the heal and stopped "+
				"advancing at %s — it is wedged, not slow, and not starved: see the peer "+
				"counts. peers: %s; disruptoor: %s", strings.Join(wedged, ", "), wantHeight,
				describeHeads(wedged, lag.heads), peers, c.disruptoorSummary())
			c.log.Error("FINDING: client wedged below the survival anchor",
				"name", sc.name, "clients", wedged, "want_height", wantHeight)
		case len(starved) > 0:
			res.Outcome = "inconclusive"
			res.Detail = fmt.Sprintf("%s never reached block %d and stopped advancing, but its "+
				"consensus client has no peers, so it is starved rather than wedged. peers: %s",
				strings.Join(starved, ", "), wantHeight, peers)
			c.log.Warn("client starved below the survival anchor", "name", sc.name, "clients", starved)
		default:
			res.Outcome = "inconclusive"
			res.Detail = fmt.Sprintf("not every client reached block %d after the heal: %s still "+
				"behind at %s, but still advancing", wantHeight, strings.Join(lag.short, ", "),
				describeHeads(lag.short, lag.heads))
		}
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
func (c *chaos) waitAgreement(ctx context.Context, timeout time.Duration) (uint64, bool, []string) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	heads := newHeadTracker()
	for {
		select {
		case <-ctx.Done():
			return 0, false, nil
		case <-tick.C:
			sample := map[string]uint64{}
			for _, e := range c.els {
				if n, _, err := e.head(ctx); err == nil {
					sample[e.name] = n
				}
			}
			heads.observe(sample)
			if frozen := heads.frozen(); len(frozen) > 0 {
				return 0, false, frozen
			}
			if time.Now().After(deadline) {
				return 0, false, nil
			}

			n, err := lowestHead(ctx, c.els)
			if err != nil || n == 0 {
				continue
			}
			if same, _ := agreed(ctx, c.els, n); same {
				return n, true, nil
			}
		}
	}
}

// wedgeTicks is how many times a client may be SEEN holding the same head, while the chain
// around it advances, before it is called wedged rather than slow.
//
// Six observations at a five-second poll is thirty seconds of standing still, spanning seven
// polls -- the first only establishes a baseline, since a stall is a comparison against a
// previous sample. That is five slots here: long enough that ordinary propagation lag never
// trips it, short enough to beat the convergence timeout by minutes.
const wedgeTicks = 6

// headTracker turns a stream of per-client head samples into "who has stopped moving".
//
// It is separate from the polling loop because this is the judgement, and the judgement is
// worth testing without a devnet attached.
type headTracker struct {
	last  map[string]uint64
	stuck map[string]int
}

func newHeadTracker() *headTracker {
	return &headTracker{last: map[string]uint64{}, stuck: map[string]int{}}
}

// observe folds one round of samples in. A client only accrues a stall when the chain around
// it moved: a devnet where nothing is proposing is a different problem, and counting it here
// would name every client as wedged the moment one of them resumed.
func (t *headTracker) observe(heads map[string]uint64) {
	moved := false
	for name, n := range heads {
		if prev, seen := t.last[name]; seen && n > prev {
			moved = true
		}
	}
	if moved {
		for name, n := range heads {
			if prev, seen := t.last[name]; seen && n == prev {
				t.stuck[name]++
			} else {
				t.stuck[name] = 0
			}
		}
	}
	for name, n := range heads {
		t.last[name] = n
	}
}

func (t *headTracker) frozen() []string {
	var out []string
	for name, n := range t.stuck {
		if n >= wedgeTicks {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// participantIndex pulls the node number out of a service name. ethereum-package names the
// two halves of one participant el-3-besu-lighthouse and cl-3-lighthouse-besu, so the index is
// what ties an execution client to its own consensus client.
func participantIndex(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// classifyFrozen splits clients that stopped advancing into those that had peers and could
// have moved -- wedged, a finding -- and those whose consensus client has none, which are
// starved by the network rather than broken. A client with an unknown peer count is treated as
// wedged: the harness would rather ask a question it cannot answer than stay quiet.
func classifyFrozen(frozen []string, byIndex map[string]int) (wedged, starved []string) {
	for _, f := range frozen {
		if n, ok := byIndex[participantIndex(f)]; ok && n == 0 {
			starved = append(starved, f)
		} else {
			wedged = append(wedged, f)
		}
	}
	return wedged, starved
}

// peerSummary reports every consensus client's peer count, both as text and by participant.
func (c *chaos) peerSummary(ctx context.Context) (string, map[string]int) {
	byIndex := map[string]int{}
	if len(c.cls) == 0 {
		return "unknown (no consensus clients given)", byIndex
	}
	var parts []string
	for _, b := range c.cls {
		n, err := b.peers(ctx)
		if err != nil {
			parts = append(parts, b.name+"=?")
			continue
		}
		byIndex[participantIndex(b.name)] = n
		parts = append(parts, fmt.Sprintf("%s=%d", b.name, n))
	}
	return strings.Join(parts, " "), byIndex
}

func (c *chaos) disruptoorSummary() string {
	p, sh, err := c.d.State()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return fmt.Sprintf("%d partition(s), %d shaping rule(s)", p, sh)
}

func (c *chaos) describeDisagreement(ctx context.Context, n uint64) string {
	_, seen := agreed(ctx, c.els, n)
	out := fmt.Sprintf("at block %d:", n)
	for name, h := range seen {
		out += fmt.Sprintf(" %s=%s", name, short(h))
	}
	return out
}
