package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// Headers are timed 6*number so slotOf6 reports the block number back as the
// slot: every test's arithmetic is in block numbers.
func slotOf6(t uint64) uint64 { return t / 6 }

func hdr(hash, parent string, number uint64) *migmon.Header {
	return &migmon.Header{Hash: hash, Parent: parent, Number: number, Time: number * 6, Root: hash + "-root"}
}

// chain returns count headers prefix<n> numbered fromNumber+1.., linked from parent.
func chain(prefix, parent string, fromNumber uint64, count int) []*migmon.Header {
	out := make([]*migmon.Header, 0, count)
	p := parent
	for i := 1; i <= count; i++ {
		n := fromNumber + uint64(i)
		h := fmt.Sprintf("%s%d", prefix, n)
		out = append(out, hdr(h, p, n))
		p = h
	}
	return out
}

func putAll(l *lineage, hs []*migmon.Header) {
	for _, h := range hs {
		l.put(h)
	}
}

func lastHash(hs []*migmon.Header) string { return hs[len(hs)-1].Hash }

// bigFork keeps every hand-minted block pre-fork unless a test sets its own.
const bigFork = 1_000_000

// newTestLineage: node 1 anchors, as in every shipped profile.
func newTestLineage(forkTime uint64) *lineage { return newLineage(forkTime, slotOf6, 100000, 1) }

// tick observes heads (node -> hash) and settles once, like the collector does.
func tick(l *lineage, now uint64, heads map[int]string) []reorg {
	for n, h := range heads {
		l.observe(n, h)
	}
	return l.settle(now)
}

func all(hash string) map[int]string { return map[int]string{1: hash, 2: hash, 3: hash, 4: hash} }

func ids(segs []segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.ID
	}
	return out
}

func find(segs []segment, id string) *segment {
	for i := range segs {
		if segs[i].ID == id {
			return &segs[i]
		}
	}
	return nil
}

func TestLineageLinearAdvanceIsOnlySpine(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 10)
	putAll(l, spine)
	for i := range spine {
		if rs := tick(l, uint64(i+1), all(spine[i].Hash)); len(rs) != 0 {
			t.Fatalf("advance to #%d: unexpected reorg %+v", i+1, rs)
		}
	}
	segs := l.segments()
	if got := ids(segs); !reflect.DeepEqual(got, []string{"s0"}) {
		t.Fatalf("segments = %v, want [s0]", got)
	}
	if segs[0].Blocks != 10 || !reflect.DeepEqual(segs[0].Holders, []int{1, 2, 3, 4}) {
		t.Fatalf("s0 = %+v, want 10 blocks held by all", segs[0])
	}
}

// A one-block lead is propagation lag, not a fork: it never becomes a branch
// while the others catch up within a tick. A two-block island is a branch on
// its second tick off-canonical; a one-block fork needs three. A lead that
// extends the anchor's own head is the canonical chain's newest block.
func TestLineageHysteresisInTicks(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 12)
	putAll(l, spine)
	tick(l, 10, all("c10"))
	tick(l, 11, map[int]string{4: "c11"}) // node 4 imports the anchor's next block first
	if seg := l.segmentOf("c11"); seg != "" {
		t.Fatalf("a one-block lead became branch %q", seg)
	}
	tick(l, 12, map[int]string{1: "c11", 2: "c11", 3: "c11"})
	if got := ids(l.segments()); !reflect.DeepEqual(got, []string{"s0"}) || l.segmentOf("c11") != "s0" {
		t.Fatalf("segments after catch-up = %v, c11 in %q, want [s0]", got, l.segmentOf("c11"))
	}

	island := chain("i", "c11", 11, 2)
	putAll(l, island)
	tick(l, 13, map[int]string{2: lastHash(island)})
	if seg := l.segmentOf(lastHash(island)); seg != "" {
		t.Fatalf("two-block island became a branch on its first tick: %q", seg)
	}
	tick(l, 14, map[int]string{})
	if seg := l.segmentOf(lastHash(island)); seg != "s1" {
		t.Fatalf("two-block island segment = %q, want s1 on the second tick", seg)
	}
	tick(l, 15, map[int]string{2: "c11"}) // back; s1 orphaned

	fork := chain("f", "c11", 11, 1)
	putAll(l, fork)
	for slot := uint64(16); slot <= 17; slot++ {
		tick(l, slot, map[int]string{3: lastHash(fork)})
		if seg := l.segmentOf(lastHash(fork)); seg != "" {
			t.Fatalf("slot %d: one-block fork recorded early as %q", slot, seg)
		}
	}
	tick(l, 18, map[int]string{})
	if seg := l.segmentOf(lastHash(fork)); seg != "s2" {
		t.Fatalf("persistent one-block fork segment = %q, want s2", seg)
	}
}

func TestLineageIslandThenRejoinOrphans(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 18)
	putAll(l, spine)
	tick(l, 10, all("c10"))
	island := chain("b", "c10", 10, 5) // b11..b15
	putAll(l, island)
	tick(l, 15, map[int]string{1: "c18", 3: "c18", 4: "c18", 2: lastHash(island)})
	tick(l, 16, map[int]string{})
	islandID := l.segmentOf(lastHash(island))
	if islandID == "" || islandID == "s0" {
		t.Fatalf("island tip segment = %q, want a fresh branch id", islandID)
	}

	rs := tick(l, 18, map[int]string{2: "c18"})
	if len(rs) != 1 {
		t.Fatalf("rejoin reorgs = %v, want exactly one", rs)
	}
	r := rs[0]
	if r.Node != 2 || r.Depth != 5 || r.Added != 8 || r.Ancestor != "c10" ||
		r.LostSegment != islandID || r.WonSegment != "s0" || r.CrossesIStar {
		t.Fatalf("reorg = %+v, want {node:2 depth:5 added:8 ancestor:c10 lost:%s won:s0 crosses:false}", r, islandID)
	}
	isl := find(l.segments(), islandID)
	if isl == nil || isl.State != "orphaned" || !reflect.DeepEqual(isl.LostBy, []int{2}) || isl.Lane != 1 {
		t.Fatalf("island after rejoin = %+v, want orphaned, lost_by [2], lane 1", isl)
	}
}

// A lane is reused once its orphan ended; a concurrent island takes the next
// lane; a live island keeps its lane however slowly it mints.
func TestLineageLanes(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 25)
	putAll(l, spine)
	tick(l, 10, all("c10"))
	island1 := chain("b", "c10", 10, 5)
	putAll(l, island1)
	tick(l, 15, map[int]string{1: "c25", 3: "c25", 4: "c25", 2: lastHash(island1)})
	tick(l, 16, map[int]string{})
	tick(l, 25, map[int]string{2: "c25"}) // rejoin: island1 orphaned, ended at 15
	if s := find(l.segments(), "s1"); s == nil || s.Lane != 1 {
		t.Fatalf("island1 = %+v, want lane 1", s)
	}

	islandC := chain("x", "c25", 25, 3)
	putAll(l, islandC)
	tick(l, 28, map[int]string{2: lastHash(islandC)})
	tick(l, 29, map[int]string{})
	if s := find(l.segments(), l.segmentOf(lastHash(islandC))); s == nil || s.Lane != 1 {
		t.Fatalf("island C = %+v, want lane 1 reused (island1 ended 3+ slots before)", s)
	}

	islandD := chain("y", "c25", 25, 2)
	putAll(l, islandD)
	tick(l, 30, map[int]string{3: lastHash(islandD)})
	tick(l, 31, map[int]string{})
	if s := find(l.segments(), l.segmentOf(lastHash(islandD))); s == nil || s.Lane != -1 {
		t.Fatalf("island D = %+v, want lane -1 (C is live on lane 1)", s)
	}
}

// Straddle: the island forks before the fork time and crosses it on its own
// block; the rewind onto the anchor's chain crosses back.
func TestLineageStraddleIStar(t *testing.T) {
	const forkTime = 20*6 + 3 // between blocks 20 and 21
	l := newTestLineage(forkTime)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 40)
	putAll(l, spine)
	tick(l, 15, all("c15"))
	island := chain("s", "c15", 15, 10) // s16..s25, crosses at s21
	putAll(l, island)
	tick(l, 25, map[int]string{1: "c40", 3: "c40", 4: "c40", 2: lastHash(island)})
	tick(l, 26, map[int]string{})
	rs := tick(l, 40, map[int]string{2: "c40"})
	if len(rs) != 1 || !rs[0].CrossesIStar {
		t.Fatalf("reorg = %v, want one reorg with crosses_istar=true", rs)
	}
	var s21, c21 *blockView
	for _, b := range l.blocks(0, 100) {
		b := b
		switch b.Hash {
		case "s21":
			s21 = &b
		case "c21":
			c21 = &b
		}
	}
	if s21 == nil || !s21.IStar || !s21.IStarOrphaned || s21.Format != "pbt" {
		t.Fatalf("island's first post-fork block = %+v, want istar, orphaned, pbt", s21)
	}
	if c21 == nil || !c21.IStar || c21.IStarOrphaned {
		t.Fatalf("spine's first post-fork block = %+v, want istar, not orphaned", c21)
	}
}

// A four-way split: the anchor's chain stays s0 while the islands' heights
// leapfrog it; at the heal every island moves onto it, each crossing back to
// the common ancestor.
func TestLineageNWaySplit(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 10)
	putAll(l, spine)
	tick(l, 10, all("c10"))
	a, b, c, d := chain("a", "c10", 10, 6), chain("b", "c10", 10, 8), chain("d", "c10", 10, 3), chain("e", "c10", 10, 3)
	for _, ch := range [][]*migmon.Header{a, b, c, d} {
		putAll(l, ch)
	}
	for k := 1; k <= 8; k++ {
		tick(l, uint64(10+k), map[int]string{
			2: b[min(k, len(b))-1].Hash, 1: a[min((k+1)/2, len(a))-1].Hash,
			3: c[min((k+2)/3, len(c))-1].Hash, 4: d[min((k+2)/3, len(d))-1].Hash,
		})
	}
	segs := l.segments()
	if !reflect.DeepEqual(segs[0].Holders, []int{1}) || segs[0].Tip[:1] != "a" {
		t.Fatalf("s0 = %+v, want the anchor's chain held by 1", segs[0])
	}
	for _, s := range segs[1:] {
		if s.State != "live" {
			t.Fatalf("phantom orphan during the split: %+v", s)
		}
	}
	if rs := l.reorgs(); len(rs) != 0 {
		t.Fatalf("phantom reorgs during the split: %+v", rs)
	}
	rs := tick(l, 20, map[int]string{2: a[3].Hash, 3: a[3].Hash, 4: a[3].Hash})
	if len(rs) != 3 {
		t.Fatalf("reorgs after the heal = %d, want one per island", len(rs))
	}
	for _, r := range rs {
		if r.WonSegment != "s0" || r.Ancestor != "c10" {
			t.Fatalf("reorg %+v, want won s0 from ancestor c10", r)
		}
	}
	orphaned := 0
	for _, s := range l.segments() {
		if s.State == "orphaned" {
			orphaned++
		}
	}
	if orphaned != 3 {
		t.Fatalf("orphaned segments = %d, want the three islands", orphaned)
	}
}

// The anchor itself moves onto a light's branch: that branch is absorbed
// into s0 (records that landed on it now say s0), the anchor's abandoned
// suffix is an orphan lost by everyone who was on it, and the anchor's own
// record is a plain reorg between the two.
func TestLineageAnchorAdoptsABranch(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	spine := chain("c", "g0", 0, 14)
	putAll(l, spine)
	tick(l, 10, all("c10"))
	tick(l, 14, map[int]string{1: "c14", 3: "c14", 4: "c14"})
	branch := chain("f", "c10", 10, 5)
	putAll(l, branch)
	tick(l, 15, map[int]string{2: lastHash(branch)})
	tick(l, 16, map[int]string{})
	if seg := l.segmentOf(lastHash(branch)); seg != "s1" {
		t.Fatalf("branch segment = %q, want s1", seg)
	}
	rs := tick(l, 17, map[int]string{1: lastHash(branch)})
	if len(rs) != 1 || rs[0].Node != 1 || rs[0].Depth != 4 || rs[0].Added != 5 || rs[0].WonSegment != "s0" {
		t.Fatalf("anchor's reorg = %+v, want node 1 depth 4 added 5 onto s0", rs)
	}
	segs := l.segments()
	if segs[0].Tip != lastHash(branch) || l.segmentOf("f12") != "s0" {
		t.Fatalf("s0 = %+v, want the adopted branch as canonical", segs[0])
	}
	if find(segs, "s1") != nil {
		t.Fatal("the adopted branch still exists as a separate segment")
	}
	orphan := find(segs, l.segmentOf("c14"))
	if orphan == nil || orphan.State != "orphaned" || orphan.Tip != "c14" || !reflect.DeepEqual(orphan.LostBy, []int{1, 3, 4}) {
		t.Fatalf("anchor's old suffix = %+v, want an orphan to c14 lost by everyone who was on it, the anchor included", orphan)
	}
	if rs[0].LostSegment != orphan.ID {
		t.Fatalf("anchor's record lost %q, want the orphan %s", rs[0].LostSegment, orphan.ID)
	}
	// Late movers land on s0 and leave the orphan.
	rs = tick(l, 18, map[int]string{3: lastHash(branch), 4: lastHash(branch)})
	for _, r := range rs {
		if r.LostSegment != orphan.ID || r.WonSegment != "s0" {
			t.Fatalf("mover %d: %+v, want lost %s won s0", r.Node, r, orphan.ID)
		}
	}
}

// A move whose common ancestor the store no longer holds is not judged: no
// record with a guessed ancestor, no uint64 underflow, one truncation counted.
func TestLineageTruncatedWalkIsNotARecord(t *testing.T) {
	l := newTestLineage(bigFork)
	x := chain("x", "unknown5", 5, 3)
	y := chain("y", "unknown5", 5, 6)
	putAll(l, x)
	putAll(l, y)
	tick(l, 8, map[int]string{1: lastHash(y), 2: lastHash(x), 3: lastHash(y), 4: lastHash(y)})
	tick(l, 9, map[int]string{})
	rs := tick(l, 10, map[int]string{2: lastHash(y)})
	if len(rs) != 0 || len(l.reorgs()) != 0 {
		t.Fatalf("reorgs = %v, want none: the ancestor is unknown", rs)
	}
	if l.truncated != 1 {
		t.Fatalf("truncated = %d, want 1", l.truncated)
	}
}

// fakeExtendClient serves headers by hash and counts calls.
type fakeExtendClient struct {
	migmon.Client
	byHash map[string]*migmon.Header
	calls  int
}

func (f *fakeExtendClient) HeaderByHash(_ context.Context, hash string) (*migmon.Header, error) {
	f.calls++
	h, ok := f.byHash[hash]
	if !ok {
		return nil, fmt.Errorf("no header for %s", hash)
	}
	return h, nil
}

func (f *fakeExtendClient) Name() string { return "fake" }

func TestLineageExtendFetchesOnlyTheUnknown(t *testing.T) {
	l := newTestLineage(bigFork)
	l.put(hdr("g0", "", 0))
	known := chain("c", "g0", 0, 5)
	putAll(l, known)
	unknown := chain("c", "c5", 5, 7) // c6..c12
	f := &fakeExtendClient{byHash: map[string]*migmon.Header{}}
	for _, h := range unknown {
		f.byHash[h.Hash] = h
	}
	got, err := l.extend(context.Background(), f, unknown[6], 100)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 6 || len(got) != 7 {
		t.Fatalf("calls = %d got = %d, want 6 fetches (c11..c6) and 7 headers incl. the head", f.calls, len(got))
	}
	f.calls = 0
	if _, err := l.extend(context.Background(), f, unknown[6], 100); err != nil || f.calls != 0 {
		t.Fatalf("second extend: calls = %d err = %v, want 0 and nil", f.calls, err)
	}
}
