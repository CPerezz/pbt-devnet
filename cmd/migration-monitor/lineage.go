package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

type block struct {
	Hash, Parent, Root string
	Number, Time       uint64
}

// lineage is the block store plus what the page derives from it: the
// anchor's chain (s0, canonical by the devnet's design - the anchor never
// loses a heal), every other node's branch off it, the orphans those
// branches become, and per-node reorgs. Heads are recorded with observe and
// folded once per tick with settle; the caller serialises.
type lineage struct {
	forkTime    uint64
	slotOf      func(unixTime uint64) uint64
	retainSlots uint64
	anchor      int

	byHash    map[string]block
	heads     map[int]string    // node -> current head
	moved     map[int]string    // node -> previous head, for heads that changed since the last settle
	off       map[int]int       // ticks a node's head has been off-canonical without a segment
	segOf     map[string]string // block hash -> segment id
	recs      map[string]*segment
	byFirst   map[string]string // first block of a branch -> segment id
	nextID    int
	log       []reorg
	truncated int // head moves left unjudged because the store lost their ancestry
}

func newLineage(forkTime uint64, slotOf func(unixTime uint64) uint64, retainSlots uint64, anchor int) *lineage {
	return &lineage{
		forkTime: forkTime, slotOf: slotOf, retainSlots: retainSlots, anchor: anchor,
		byHash: map[string]block{}, heads: map[int]string{}, moved: map[int]string{}, off: map[int]int{},
		segOf: map[string]string{}, recs: map[string]*segment{}, byFirst: map[string]string{}, nextID: 1,
	}
}

// put records a header; a hash already known is left untouched.
func (l *lineage) put(h *migmon.Header) {
	if _, ok := l.byHash[h.Hash]; !ok {
		l.byHash[h.Hash] = block{Hash: h.Hash, Parent: h.Parent, Root: h.Root, Number: h.Number, Time: h.Time}
	}
}

// extend records head and walks its parents, fetching each unknown one from c
// until a known block, genesis, or maxDepth fetches. Returns head plus what
// was fetched, newest first; a fetch error returns what it has so far.
func (l *lineage) extend(ctx context.Context, c migmon.Client, head *migmon.Header, maxDepth int) ([]*migmon.Header, error) {
	l.put(head)
	got := []*migmon.Header{head}
	cur := l.byHash[head.Hash]
	for len(got)-1 < maxDepth && cur.Number > 0 {
		if _, known := l.byHash[cur.Parent]; known {
			break
		}
		h, err := c.HeaderByHash(ctx, cur.Parent)
		if err != nil {
			return got, err
		}
		if h == nil {
			return got, fmt.Errorf("parent %s of block %d is unknown to %s", cur.Parent, cur.Number, c.Name())
		}
		l.put(h)
		got = append(got, h)
		cur = l.byHash[h.Hash]
	}
	return got, nil
}

// observe records node's head (already put/extended); settle folds it in.
func (l *lineage) observe(node int, head string) {
	prev, seen := l.heads[node]
	if seen && prev == head {
		return
	}
	if _, pending := l.moved[node]; !pending {
		l.moved[node] = prev
	}
	l.heads[node] = head
}

// settle recomputes the segments once for this tick and returns the reorgs
// the moves since the last settle imply. A head whose previous head is one
// of its ancestors is a plain advance, not a reorg.
func (l *lineage) settle(now uint64) []reorg {
	moved := l.moved
	l.moved = map[int]string{}
	l.recompute(moved)
	nodes := make([]int, 0, len(moved))
	for n := range moved {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	var out []reorg
	for _, node := range nodes {
		prev, head := moved[node], l.heads[node]
		if prev == "" || l.isAncestor(prev, head) {
			continue
		}
		a, ok := l.commonAncestor(prev, head)
		p, n := l.byHash[prev], l.byHash[head]
		if !ok || a.Number > p.Number || a.Number > n.Number {
			l.truncated++
			continue
		}
		r := reorg{
			Slot: now, Node: node,
			Depth: p.Number - a.Number, Added: n.Number - a.Number,
			Ancestor: a.Hash, AncestorNumber: a.Number,
			OldHead: prev, NewHead: head,
			LostSegment: l.segOf[prev], WonSegment: l.segOf[head],
			CrossesIStar: a.Time < l.forkTime && p.Time >= l.forkTime,
		}
		l.log = append(l.log, r)
		out = append(out, r)
	}
	return out
}

// recompute rebuilds s0 from the anchor's head and every branch from the
// other heads. moved maps the nodes whose head changed to their previous
// head, so an anchor move can name who was on its abandoned suffix.
func (l *lineage) recompute(moved map[int]string) {
	tip, ok := l.heads[l.anchor]
	if _, known := l.byHash[tip]; !ok || !known {
		return
	}
	canonical := map[string]bool{}
	first := tip
	for cur := tip; ; {
		canonical[cur] = true
		first = cur
		b := l.byHash[cur]
		if _, known := l.byHash[b.Parent]; !known {
			break
		}
		cur = b.Parent
	}
	s0 := l.recs["s0"]
	if s0 == nil {
		s0 = &segment{ID: "s0", State: "live"}
		l.recs["s0"] = s0
	}
	// The anchor moved onto another chain: its old suffix is an orphan, lost
	// by whoever was on it, and records that left it during its s0 days
	// point at the orphan, not lane 0.
	if old := s0.Tip; old != "" && !canonical[old] {
		if suffix := l.walkTo(old, canonical); len(suffix) > 0 {
			id := l.segmentID(suffix[len(suffix)-1])
			for _, h := range suffix {
				l.segOf[h] = id
			}
			var lostBy []int
			for n, h := range l.heads {
				if prev, ok := moved[n]; ok {
					h = prev
				}
				if l.segOf[h] == id {
					lostBy = append(lostBy, n)
				}
			}
			sort.Ints(lostBy)
			for i := range l.log {
				if l.log[i].LostSegment == "s0" && l.segOf[l.log[i].OldHead] == id {
					l.log[i].LostSegment = id
				}
			}
			rec := l.recs[id]
			rec.Ancestor = l.byHash[suffix[len(suffix)-1]].Parent
			rec.Tip, rec.Blocks, rec.LastSlot = old, uint64(len(suffix)), l.slotOf(l.byHash[old].Time)
			rec.State, rec.Holders, rec.LostBy = "orphaned", nil, lostBy
		}
	}
	for h := range canonical {
		l.segOf[h] = "s0"
	}
	t := l.byHash[tip]
	s0.Tip, s0.Blocks = tip, t.Number
	s0.FirstSlot, s0.LastSlot = l.slotOf(l.byHash[first].Time), l.slotOf(t.Time)
	s0.Holders = s0.Holders[:0]

	seen := map[string]bool{"s0": true}
	nodes := make([]int, 0, len(l.heads))
	for n := range l.heads {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	for _, n := range nodes {
		h := l.heads[n]
		if canonical[h] {
			s0.Holders = append(s0.Holders, n)
			l.off[n] = 0
			continue
		}
		branch := l.walkTo(h, canonical)
		if len(branch) == 0 {
			continue
		}
		firstHash := branch[len(branch)-1]
		if _, known := l.byFirst[firstHash]; !known {
			// Hysteresis: the first importer of a block is alone on it for a
			// tick; a branch is a head still off-canonical next tick and two
			// blocks deep, or a one-block fork that outlives two ticks.
			l.off[n]++
			if l.off[n] < 2 || (len(branch) < 2 && l.off[n] < 3) {
				continue
			}
		}
		id := l.segmentID(firstHash)
		rec := l.recs[id]
		b := l.byHash[h]
		if rec.State == "orphaned" {
			// Parked on a lost tip: it will move. Minting past the tip means the branch lives.
			if b.Number <= l.byHash[rec.Tip].Number {
				continue
			}
			rec.State, rec.LostBy = "live", nil
		}
		for _, bh := range branch {
			l.segOf[bh] = id
		}
		if !seen[id] {
			seen[id] = true
			rec.Holders, rec.Tip, rec.Blocks = rec.Holders[:0], "", 0
		}
		if rec.Tip == "" || b.Number > l.byHash[rec.Tip].Number {
			rec.Tip, rec.LastSlot = h, l.slotOf(b.Time)
			rec.Ancestor = l.byHash[firstHash].Parent
			rec.Blocks = b.Number - l.byHash[firstHash].Number + 1
		}
		rec.Holders = append(rec.Holders, n)
	}

	for id, rec := range l.recs {
		if seen[id] {
			continue
		}
		if first := l.firstOf(id); canonical[first] {
			// The anchor adopted this branch: the moves that landed on it landed on s0.
			for i := range l.log {
				if l.log[i].WonSegment == id {
					l.log[i].WonSegment = "s0"
				}
				if l.log[i].LostSegment == id {
					l.log[i].LostSegment = "s0"
				}
			}
			delete(l.recs, id)
			delete(l.byFirst, first)
			continue
		}
		if rec.State == "live" {
			rec.State, rec.LostBy, rec.Holders = "orphaned", rec.Holders, nil
		}
	}
}

// walkTo returns hash and its ancestors back to, not including, the first
// canonical block, newest first; empty when hash is canonical or unknown.
func (l *lineage) walkTo(hash string, canonical map[string]bool) []string {
	var out []string
	for cur := hash; ; {
		b, known := l.byHash[cur]
		if !known || canonical[cur] {
			return out
		}
		out = append(out, cur)
		cur = b.Parent
	}
}

// segmentID returns the segment a branch's first block identifies, allocating
// an id and a lane on first sighting.
func (l *lineage) segmentID(firstHash string) string {
	if id, ok := l.byFirst[firstHash]; ok {
		return id
	}
	id := fmt.Sprintf("s%d", l.nextID)
	l.nextID++
	l.byFirst[firstHash] = id
	firstSlot := l.slotOf(l.byHash[firstHash].Time)
	l.recs[id] = &segment{ID: id, Lane: l.freeLane(firstSlot), FirstSlot: firstSlot, State: "live"}
	return id
}

func (l *lineage) firstOf(id string) string {
	for h, sid := range l.byFirst {
		if sid == id {
			return h
		}
	}
	return ""
}

// freeLane: 1,-1,2,-2,3,-3 with no live occupant and every orphaned one ended
// more than three slots before firstSlot; 4 when none is free.
func (l *lineage) freeLane(firstSlot uint64) int {
	for _, lane := range []int{1, -1, 2, -2, 3, -3} {
		free := true
		for _, rec := range l.recs {
			if rec.Lane == lane && (rec.State == "live" || rec.LastSlot+3 >= firstSlot) {
				free = false
				break
			}
		}
		if free {
			return lane
		}
	}
	return 4
}

// isAncestor reports whether a is b or one of b's known ancestors.
func (l *lineage) isAncestor(a, b string) bool {
	ab, ok := l.byHash[a]
	if !ok {
		return false
	}
	cur, ok := l.byHash[b]
	for ok && cur.Number > ab.Number {
		cur, ok = l.byHash[cur.Parent]
	}
	return ok && cur.Hash == a
}

// commonAncestor walks the higher block down to equal numbers, then both down
// until the hashes match. An unknown parent on the way means the answer is
// not in the store: false, never a guess.
func (l *lineage) commonAncestor(x, y string) (block, bool) {
	bx, okx := l.byHash[x]
	by, oky := l.byHash[y]
	if !okx || !oky {
		return block{}, false
	}
	for bx.Number > by.Number {
		if bx, okx = l.byHash[bx.Parent]; !okx {
			return block{}, false
		}
	}
	for by.Number > bx.Number {
		if by, oky = l.byHash[by.Parent]; !oky {
			return block{}, false
		}
	}
	for bx.Hash != by.Hash {
		px, okx := l.byHash[bx.Parent]
		py, oky := l.byHash[by.Parent]
		if !okx || !oky {
			return block{}, false
		}
		bx, by = px, py
	}
	return bx, true
}

// segments returns s0 first, then branches in order of first sighting.
func (l *lineage) segments() []segment {
	ids := make([]string, 0, len(l.recs))
	for id := range l.recs {
		if id != "s0" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return segNum(ids[i]) < segNum(ids[j]) })
	if l.recs["s0"] != nil {
		ids = append([]string{"s0"}, ids...)
	}
	out := make([]segment, 0, len(ids))
	for _, id := range ids {
		s := *l.recs[id]
		s.Holders, s.LostBy = nonNil(append([]int(nil), s.Holders...)), nonNil(append([]int(nil), s.LostBy...))
		out = append(out, s)
	}
	return out
}

func segNum(id string) int {
	var n int
	fmt.Sscanf(id, "s%d", &n)
	return n
}

func (l *lineage) reorgs() []reorg { return append([]reorg(nil), l.log...) }

func (l *lineage) segmentOf(hash string) string { return l.segOf[hash] }

// blocks returns the known blocks with fromSlot <= slot <= toSlot by number
// then hash; shadow fields are left for the shadow table.
func (l *lineage) blocks(fromSlot, toSlot uint64) []blockView {
	var out []blockView
	for _, b := range l.byHash {
		slot := l.slotOf(b.Time)
		if slot < fromSlot || slot > toSlot {
			continue
		}
		v := blockView{
			Hash: b.Hash, Parent: b.Parent, Number: b.Number, Slot: slot,
			Segment: l.segOf[b.Hash], Format: "mpt", StateRoot: b.Root,
		}
		if b.Time >= l.forkTime {
			v.Format = "pbt"
			if p, ok := l.byHash[b.Parent]; ok && p.Time < l.forkTime {
				v.IStar = true
				if rec := l.recs[v.Segment]; rec != nil && rec.State == "orphaned" {
					v.IStarOrphaned = true
				}
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Number != out[j].Number {
			return out[i].Number < out[j].Number
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}

// holders returns the nodes whose head is on a live segment.
func (l *lineage) holders(segmentID string) []int {
	rec := l.recs[segmentID]
	if rec == nil || rec.State != "live" {
		return nil
	}
	return append([]int(nil), rec.Holders...)
}

// prune drops blocks and reorg records older than retainSlots, keeping every
// block a segment record or a head still points at so later walks stay anchored.
func (l *lineage) prune(nowSlot uint64) {
	if nowSlot < l.retainSlots {
		return
	}
	cutoff := nowSlot - l.retainSlots
	keep := map[string]bool{}
	for _, rec := range l.recs {
		keep[rec.Tip], keep[rec.Ancestor] = true, true
	}
	for _, h := range l.heads {
		keep[h] = true
	}
	for hash, b := range l.byHash {
		if l.slotOf(b.Time) < cutoff && !keep[hash] {
			delete(l.byHash, hash)
			delete(l.segOf, hash)
		}
	}
	kept := l.log[:0]
	for _, r := range l.log {
		if r.Slot >= cutoff {
			kept = append(kept, r)
		}
	}
	l.log = kept
}
