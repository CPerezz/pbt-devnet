package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// The per-client comparison behind the page's client table: every node's
// answer at ONE height, judged against a reference.
//
// Comparing each node at its own head says nothing - heads legitimately differ
// by a block or two - so the whole table is anchored at a single height and
// every cell is that node's answer there. The height sits `compareDepth` below
// the lowest eligible head because the tip is the least settled place on the
// chain: a one-block reorg at the tip would otherwise paint a node red for
// behaving correctly.
const compareDepth = 2

type cellState string

const (
	cellAgree       cellState = "agree"
	cellDissent     cellState = "dissent"
	cellUnjudged    cellState = "unjudged"    // consistent with this node's own block, so not comparable
	cellNoIntrospec cellState = "noinspect"   // the client has no such RPC; absence is its contract
	cellPending     cellState = "pending"     // introspecting, but nothing recorded for this block yet
	cellBehind      cellState = "behind"      // the node has not reached the height
	cellUnreachable cellState = "unreachable" // the node did not answer at all
)

// Verdicts, worst first. A fork absorbs an I* mismatch: one disagreement
// reported once.
const (
	verdictHeaderDissent = "header-dissent"
	verdictShadowDissent = "shadow-dissent"
	verdictIStarDissent  = "istar-dissent"
	verdictFork          = "fork"
	verdictPartial       = "partial"
	verdictAbsent        = "absent"
	verdictOK            = "ok"
)

// nodeAnswer is one node's reading at the comparison height, as the collector
// observed it this tick.
type nodeAnswer struct {
	Node        int
	HeadNumber  uint64
	Status      string // ok|unreachable
	Isolated    bool
	Introspects bool
	Settling    string // non-empty: why this node's disagreement is expected right now
	Phase       string

	Hash       string // canonical hash at the height, "" = no answer
	HeaderRoot string // header (primary) root at the height
	ShadowRoot string // the other format's root for that block, "" = not recorded
	IStarHash  string // the fork block this node crossed on, "" = not crossed yet
}

// pickHeight is the highest height every settled node can be expected to have.
// A node that is isolated, unreachable or stalled is excluded rather than
// allowed to pin the comparison to its own stale head, and shows up in the
// table as behind.
func pickHeight(answers []nodeAnswer) (uint64, bool) {
	var lowest uint64
	found := false
	for _, a := range answers {
		if a.Status != "ok" || a.Isolated || a.Settling != "" || a.Phase == "stalled" || a.Phase == "unknown" {
			continue
		}
		if !found || a.HeadNumber < lowest {
			lowest, found = a.HeadNumber, true
		}
	}
	if !found {
		return 0, false
	}
	if lowest < compareDepth {
		return 0, true
	}
	return lowest - compareDepth, true
}

// judge turns the nodes' answers at one height into the table the page draws.
// The reference is the anchor's answer when it has one - the anchor is never
// partitioned, so by this devnet's design its chain is the canonical one - and
// a strict majority otherwise. With neither, nothing is judged and nothing is
// coloured: an unknown reference must not manufacture dissent.
func judge(height uint64, answers []nodeAnswer, anchor int) compareView {
	v := compareView{Height: height, Rows: make([]compareRow, 0, len(answers)), RefSource: "none"}

	counts := map[string]int{}
	var anchorHash string
	for _, a := range answers {
		if a.Hash == "" {
			continue
		}
		if a.Node == anchor {
			anchorHash = a.Hash
		}
		if !a.Isolated && a.Settling == "" {
			counts[a.Hash]++
		}
	}
	switch {
	case anchorHash != "":
		v.Ref, v.RefSource = anchorHash, "anchor"
	default:
		if h, ok := strictMajority(counts); ok {
			v.Ref, v.RefSource = h, "majority"
		}
	}

	// Roots are only comparable between nodes holding the reference block:
	// a node on another branch has its own consistent roots, and colouring
	// them would report one fork three times.
	onRef := func(a nodeAnswer) bool { return v.Ref != "" && a.Hash == v.Ref }
	headerClass := majorityRoot(answers, anchor, func(a nodeAnswer) string {
		if !onRef(a) {
			return ""
		}
		return a.HeaderRoot
	})
	shadowClass := majorityRoot(answers, anchor, func(a nodeAnswer) string {
		if !onRef(a) || !a.Introspects {
			return ""
		}
		return a.ShadowRoot
	})
	var anchorIStar string
	for _, a := range answers {
		if a.Node == anchor {
			anchorIStar = a.IStarHash
		}
	}

	shadowSeen := map[string]bool{}
	for _, a := range answers {
		r := compareRow{
			Node: a.Node, Hash: a.Hash, HeaderRoot: a.HeaderRoot, ShadowRoot: a.ShadowRoot,
			Ahead: int64(a.HeadNumber) - int64(height), Expected: a.Isolated || a.Settling != "", Why: a.Settling,
		}
		if a.Isolated && r.Why == "" {
			r.Why = "isolated"
		}

		switch {
		case a.Status != "ok":
			r.HashState, r.HeaderState, r.ShadowState = cellUnreachable, cellUnreachable, cellUnreachable
		case a.Hash == "":
			state := cellBehind
			if a.HeadNumber >= height {
				state = cellPending
			}
			r.HashState, r.HeaderState, r.ShadowState = state, state, state
		default:
			r.HashState = cellUnjudged
			if v.Ref != "" {
				r.HashState = cellAgree
				if a.Hash != v.Ref {
					r.HashState = cellDissent
				}
			}
			r.HeaderState = rootState(a.HeaderRoot, headerClass, onRef(a), true)
			r.ShadowState = rootState(a.ShadowRoot, shadowClass, onRef(a), a.Introspects)
			if onRef(a) && a.Introspects && a.ShadowRoot != "" {
				shadowSeen[a.ShadowRoot] = true
			}
		}

		r.IStarState = cellUnjudged
		if a.IStarHash != "" && anchorIStar != "" {
			r.IStarState = cellAgree
			if a.IStarHash != anchorIStar {
				r.IStarState = cellDissent
			}
		}

		r.Verdict = verdictFor(r)
		v.Rows = append(v.Rows, r)

		if r.HashState == cellAgree || r.HashState == cellDissent {
			v.HashJudged++
			if r.HashState == cellAgree {
				v.HashAgree++
			}
		}
		if r.ShadowState == cellAgree || r.ShadowState == cellDissent {
			v.ShadowJudged++
			if r.ShadowState == cellAgree {
				v.ShadowAgree++
			}
		}
	}
	sort.Slice(v.Rows, func(i, j int) bool { return v.Rows[i].Node < v.Rows[j].Node })
	v.ShadowClasses = len(shadowSeen)
	return v
}

// rootState judges one root cell. A client with no such RPC reads as its own
// contract, never as a fault.
func rootState(root, class string, comparable, introspects bool) cellState {
	switch {
	case !introspects:
		return cellNoIntrospec
	case !comparable:
		return cellUnjudged
	case root == "":
		return cellPending
	case class == "":
		return cellUnjudged
	case root == class:
		return cellAgree
	default:
		return cellDissent
	}
}

// verdictFor reports the worst thing true of one row.
func verdictFor(r compareRow) string {
	switch {
	case r.HeaderState == cellDissent:
		return verdictHeaderDissent
	case r.ShadowState == cellDissent:
		return verdictShadowDissent
	case r.HashState == cellDissent:
		// A node on another branch crossed I* on its own block; that is the
		// fork, not a second finding.
		return verdictFork
	case r.IStarState == cellDissent:
		return verdictIStarDissent
	case r.HashState == cellUnreachable:
		return verdictAbsent
	case r.HashState == cellBehind:
		return verdictAbsent
	case r.HashState == cellPending || r.ShadowState == cellPending ||
		r.ShadowState == cellNoIntrospec || r.HeaderState == cellPending:
		return verdictPartial
	default:
		return verdictOK
	}
}

// majorityRoot is the class a root cell is judged against: the most common
// value, with the anchor breaking ties because its chain is the canonical one.
func majorityRoot(answers []nodeAnswer, anchor int, pick func(nodeAnswer) string) string {
	counts := map[string]int{}
	var anchorVal string
	for _, a := range answers {
		v := pick(a)
		if v == "" {
			continue
		}
		counts[v]++
		if a.Node == anchor {
			anchorVal = v
		}
	}
	best, bestN, tied := "", 0, false
	for v, n := range counts {
		switch {
		case n > bestN:
			best, bestN, tied = v, n, false
		case n == bestN && v != best:
			tied = true
		}
	}
	if tied && anchorVal != "" {
		return anchorVal
	}
	return best
}

func strictMajority(counts map[string]int) (string, bool) {
	total := 0
	for _, n := range counts {
		total += n
	}
	for v, n := range counts {
		if n*2 > total {
			return v, true
		}
	}
	return "", false
}

// compare reads every node at one height and judges the answers. One extra
// header read per node per tick; the shadow root comes from the table the
// page already fills. An unexpected dissent that the per-block shadow check
// cannot see - two clients serving different roots for the SAME block, or
// crossing the fork on different blocks - is emitted as a finding, so the
// table is evidence a lap can fail on rather than decoration.
func (c *collector) compare(ctx context.Context, log *migmon.Log, nodes []nodeView, reorgs []reorg, now uint64) compareView {
	grace := migmon.ConvergenceGrace / max64(c.slotSeconds, 1)
	answers := make([]nodeAnswer, 0, len(nodes))
	for _, n := range nodes {
		a := nodeAnswer{
			Node: n.ID, HeadNumber: n.HeadNumber, Status: n.Status, Isolated: n.Isolated,
			Introspects: c.states[n.ID-1].rpc.Introspects(), Phase: n.Phase,
			Settling: c.settling(n.ID, reorgs, now, grace),
		}
		if rec := c.states[n.ID-1].timeline.IStarRecord(); rec != nil {
			a.IStarHash = rec.Hash
		}
		answers = append(answers, a)
	}
	height, ok := pickHeight(answers)
	if !ok {
		return compareView{RefSource: "none", Rows: []compareRow{}}
	}
	for i := range answers {
		if answers[i].Status != "ok" {
			continue
		}
		hdr, err := c.states[answers[i].Node-1].rpc.HeaderByNumber(ctx, height)
		if err != nil || hdr == nil {
			continue // behind, pruned, or a transient miss: judged as such
		}
		answers[i].Hash, answers[i].HeaderRoot = hdr.Hash, hdr.Root
		answers[i].ShadowRoot = c.shadow.roots(hdr.Hash)[fmt.Sprint(answers[i].Node)]
	}
	v := judge(height, answers, c.anchor)
	c.reportDissent(log, v, now)
	return v
}

// reportDissent emits the findings the rest of the harness cannot derive,
// once per node per height. A fork is already reported by the lineage and a
// shadow split by the per-block check, so neither is repeated here.
func (c *collector) reportDissent(log *migmon.Log, v compareView, now uint64) {
	if c.cmpFired == nil {
		c.cmpFired = map[string]bool{}
	}
	for _, r := range v.Rows {
		if r.Expected || (r.Verdict != verdictHeaderDissent && r.Verdict != verdictIStarDissent) {
			continue
		}
		key := fmt.Sprintf("%d/%d/%s", r.Node, v.Height, r.Verdict)
		if c.cmpFired[key] {
			continue
		}
		c.cmpFired[key] = true
		detail := fmt.Sprintf("header root %s at block #%d, where the other clients hold the same hash %s",
			r.HeaderRoot, v.Height, shortHash(v.Ref))
		if r.Verdict == verdictIStarDissent {
			detail = fmt.Sprintf("crossed the fork on a different block than the anchor (#%d)", v.Height)
		}
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Node: c.states[r.Node-1].name, Number: v.Height,
			Finding: migmon.FindingRootMismatch, Detail: detail,
		})
	}
	// The keys are per height, so the map would grow with the chain.
	if len(c.cmpFired) > 4096 {
		c.cmpFired = map[string]bool{}
	}
}

func shortHash(h string) string {
	if len(h) > 14 {
		return h[:10] + "…" + h[len(h)-4:]
	}
	return h
}

// settling reports why a node's disagreement is expected right now: it is
// inside the grace after its own partition lifted, or after its own reorg.
// Judging those as findings would cry wolf through most of a lap.
func (c *collector) settling(node int, reorgs []reorg, now, grace uint64) string {
	if c.parts != nil {
		for _, p := range c.parts.list() {
			if p.LiftedSlot == 0 || !containsInt(p.Victims, node) {
				continue
			}
			if now >= p.LiftedSlot && now-p.LiftedSlot <= grace {
				return fmt.Sprintf("lifted at slot %d", p.LiftedSlot)
			}
		}
	}
	for _, r := range reorgs {
		if r.Node == node && now >= r.Slot && now-r.Slot <= grace {
			return fmt.Sprintf("reorg at slot %d", r.Slot)
		}
	}
	return ""
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
