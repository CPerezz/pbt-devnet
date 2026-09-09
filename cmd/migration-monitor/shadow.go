package main

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// shadowTable collects, per tracked block, the shadow root every introspecting
// node reports for it, with a bounded number of RPCs per node per tick. The
// caller serialises.
type shadowTable struct {
	nodes       []migmon.Client // index i is participant i+1
	retryEvery  time.Duration
	retainSlots uint64
	blocks      map[string]*shadowBlock
	warned      map[string]bool // node|error already reported once
}

type shadowBlock struct {
	slot  uint64
	cells []shadowCell // one per node
}

// shadowCell: a non-empty root is final and never asked again.
type shadowCell struct {
	root      string
	lastAsked time.Time
	asks      int
}

// backoffCap: retryEvery doubles per unanswered ask up to 2^5, so a block a
// node will never report costs one RPC every few minutes, not every retry.
const backoffCap = 5

func newShadowTable(nodes []migmon.Client, retryEvery time.Duration, retainSlots uint64) *shadowTable {
	return &shadowTable{nodes: nodes, retryEvery: retryEvery, retainSlots: retainSlots,
		blocks: map[string]*shadowBlock{}, warned: map[string]bool{}}
}

// track adds a block to sample; re-tracking keeps its answers.
func (t *shadowTable) track(hash string, slot uint64) {
	if _, ok := t.blocks[hash]; !ok {
		t.blocks[hash] = &shadowBlock{slot: slot, cells: make([]shadowCell, len(t.nodes))}
	}
}

// step drops blocks past retention, then asks each introspecting node for at
// most budget unanswered blocks, newest first, honouring per-pair backoff. ""
// (a legal null) stays unreported; an error is warned once per node and
// message - a restarted follower fails on every block it never saw.
func (t *shadowTable) step(ctx context.Context, log *migmon.Log, now time.Time, nowSlot uint64, budget int) {
	pending := make([]string, 0, len(t.blocks))
	for hash, b := range t.blocks {
		if b.slot+t.retainSlots < nowSlot {
			delete(t.blocks, hash)
			continue
		}
		pending = append(pending, hash)
	}
	sort.Slice(pending, func(i, j int) bool {
		bi, bj := t.blocks[pending[i]], t.blocks[pending[j]]
		if bi.slot != bj.slot {
			return bi.slot > bj.slot
		}
		return pending[i] < pending[j]
	})
	for i, n := range t.nodes {
		if !n.Introspects() {
			continue
		}
		asked := 0
		for _, hash := range pending {
			if asked >= budget {
				break
			}
			c := &t.blocks[hash].cells[i]
			if c.root != "" || (!c.lastAsked.IsZero() && now.Sub(c.lastAsked) < t.retryEvery<<min(c.asks-1, backoffCap)) {
				continue
			}
			c.lastAsked = now
			c.asks++
			asked++
			root, err := n.ShadowRoot(ctx, hash)
			if err != nil {
				if key := n.Name() + "|" + err.Error(); !t.warned[key] {
					t.warned[key] = true
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: n.Name(), Hash: hash, Detail: "shadow root: " + err.Error()})
				}
				continue
			}
			c.root = root
		}
	}
}

// roots returns node id (decimal, the JSON key) -> root for the reported pairs.
func (t *shadowTable) roots(hash string) map[string]string {
	out := map[string]string{}
	b := t.blocks[hash]
	if b == nil {
		return out
	}
	for i, c := range b.cells {
		if c.root != "" {
			out[strconv.Itoa(i+1)] = c.root
		}
	}
	return out
}

// agreement classifies a block's reports: gone when orphaned; pending while
// nobody holds it or nothing is in yet; two distinct roots is a split, with
// dissent = the nodes off the majority root (tie -> the lowest node's root);
// none when no holder runs a shadow any more (all done); else single/all/
// partial by coverage of the expected reporters.
func agreement(roots map[string]string, holders, expected []int, orphaned bool) (state string, dissent []int) {
	if orphaned {
		return "gone", nil
	}
	if len(holders) == 0 || (len(roots) == 0 && len(expected) > 0) {
		return "pending", nil
	}
	ids := make([]int, 0, len(roots))
	for k := range roots {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	count := map[string]int{}
	for _, id := range ids {
		count[roots[strconv.Itoa(id)]]++
	}
	if len(count) > 1 {
		majority := ""
		for _, id := range ids { // ascending, so a tie keeps the lowest id's root
			r := roots[strconv.Itoa(id)]
			if majority == "" || count[r] > count[majority] {
				majority = r
			}
		}
		for _, id := range ids {
			if roots[strconv.Itoa(id)] != majority {
				dissent = append(dissent, id)
			}
		}
		return "split", dissent
	}
	if len(expected) == 0 {
		return "none", nil
	}
	reported := 0
	for _, id := range expected {
		if _, ok := roots[strconv.Itoa(id)]; ok {
			reported++
		}
	}
	switch {
	case len(expected) == 1 && reported == 1:
		return "single", nil
	case reported == len(expected):
		return "all", nil
	default:
		return "partial", nil
	}
}
