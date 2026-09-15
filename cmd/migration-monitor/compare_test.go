package main

import "testing"

// The client table's whole value is that a red cell means something. These
// pin the judge against the ways a healthy lap produces disagreement.

func ok(node int, hash, header, shadow string) nodeAnswer {
	return nodeAnswer{Node: node, HeadNumber: 100, Status: "ok", Introspects: true, Phase: "synced",
		Hash: hash, HeaderRoot: header, ShadowRoot: shadow}
}

func rowOf(t *testing.T, v compareView, node int) compareRow {
	t.Helper()
	for _, r := range v.Rows {
		if r.Node == node {
			return r
		}
	}
	t.Fatalf("node %d missing from the table", node)
	return compareRow{}
}

func TestJudgeAllAgree(t *testing.T) {
	v := judge(98, []nodeAnswer{
		ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"),
		ok(3, "0xaa", "0xr", "0xs"), ok(4, "0xaa", "0xr", "0xs"),
	}, 1)
	if v.RefSource != "anchor" || v.Ref != "0xaa" {
		t.Fatalf("reference: got %s/%s", v.RefSource, v.Ref)
	}
	if v.HashAgree != 4 || v.HashJudged != 4 || v.ShadowAgree != 4 || v.ShadowClasses != 1 {
		t.Fatalf("counts: %+v", v)
	}
	for _, r := range v.Rows {
		if r.Verdict != verdictOK {
			t.Fatalf("node %d: %s", r.Node, r.Verdict)
		}
	}
}

func TestJudgeMinorityForkIsUnexpected(t *testing.T) {
	v := judge(98, []nodeAnswer{
		ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"),
		ok(3, "0xaa", "0xr", "0xs"), ok(4, "0xbb", "0xq", "0xt"),
	}, 1)
	r := rowOf(t, v, 4)
	if r.HashState != cellDissent || r.Verdict != verdictFork || r.Expected {
		t.Fatalf("victim row: %+v", r)
	}
	// Its roots belong to another block, so they are not a second finding.
	if r.HeaderState != cellUnjudged || r.ShadowState != cellUnjudged {
		t.Fatalf("off-reference roots judged: %+v", r)
	}
	if v.HashAgree != 3 || v.HashJudged != 4 {
		t.Fatalf("counts: %+v", v)
	}
}

func TestJudgeIsolatedDissentIsExpected(t *testing.T) {
	victim := ok(4, "0xbb", "0xq", "0xt")
	victim.Isolated = true
	v := judge(98, []nodeAnswer{ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"), ok(3, "0xaa", "0xr", "0xs"), victim}, 1)
	r := rowOf(t, v, 4)
	if r.HashState != cellDissent || !r.Expected || r.Why != "isolated" {
		t.Fatalf("isolated victim: %+v", r)
	}
}

func TestJudgeSettlingDissentIsExpected(t *testing.T) {
	victim := ok(4, "0xbb", "0xq", "0xt")
	victim.Settling = "lifted at slot 300"
	v := judge(98, []nodeAnswer{ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"), ok(3, "0xaa", "0xr", "0xs"), victim}, 1)
	if r := rowOf(t, v, 4); !r.Expected || r.Why != "lifted at slot 300" {
		t.Fatalf("settling victim: %+v", r)
	}
}

// A node that is out of the count must not be able to deny a reference to
// everyone else, and must not be judged against a chain it left.
func TestJudgeTieIsBrokenByTheAnchor(t *testing.T) {
	v := judge(98, []nodeAnswer{
		ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"),
		ok(3, "0xbb", "0xq", "0xt"), ok(4, "0xbb", "0xq", "0xt"),
	}, 1)
	if v.RefSource != "anchor" || v.Ref != "0xaa" {
		t.Fatalf("tie: got %s/%s", v.RefSource, v.Ref)
	}
	if rowOf(t, v, 3).HashState != cellDissent || rowOf(t, v, 1).HashState != cellAgree {
		t.Fatalf("tie rows: %+v", v.Rows)
	}
}

func TestJudgeMajorityWhenTheAnchorIsSilent(t *testing.T) {
	silent := nodeAnswer{Node: 1, HeadNumber: 100, Status: "unreachable", Introspects: true}
	v := judge(98, []nodeAnswer{silent, ok(2, "0xaa", "0xr", "0xs"), ok(3, "0xaa", "0xr", "0xs"), ok(4, "0xbb", "0xq", "0xt")}, 1)
	if v.RefSource != "majority" || v.Ref != "0xaa" {
		t.Fatalf("majority: got %s/%s", v.RefSource, v.Ref)
	}
	r := rowOf(t, v, 1)
	if r.HashState != cellUnreachable || r.Verdict != verdictAbsent {
		t.Fatalf("silent anchor row: %+v", r)
	}
}

// Two against two with no anchor answer: an unknown reference must not
// manufacture dissent - half the table would be wrong.
func TestJudgeNoReferenceJudgesNothing(t *testing.T) {
	silent := nodeAnswer{Node: 1, HeadNumber: 100, Status: "unreachable", Introspects: true}
	v := judge(98, []nodeAnswer{silent, ok(2, "0xaa", "0xr", "0xs"), ok(3, "0xbb", "0xq", "0xt"), ok(4, "0xcc", "0xp", "0xu")}, 1)
	if v.RefSource != "none" || v.Ref != "" {
		t.Fatalf("reference: %s/%s", v.RefSource, v.Ref)
	}
	for _, r := range v.Rows {
		if r.HashState == cellDissent {
			t.Fatalf("dissent without a reference: %+v", r)
		}
	}
	if v.HashJudged != 0 {
		t.Fatalf("judged %d rows with no reference", v.HashJudged)
	}
}

// The signal the table exists for: same block, different header root. That is
// a client serving a root its own block hash does not commit to.
func TestJudgeHeaderRootDissent(t *testing.T) {
	v := judge(98, []nodeAnswer{
		ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"),
		ok(3, "0xaa", "0xr", "0xs"), ok(4, "0xaa", "0xWRONG", "0xs"),
	}, 1)
	r := rowOf(t, v, 4)
	if r.HeaderState != cellDissent || r.Verdict != verdictHeaderDissent || r.Expected {
		t.Fatalf("header dissent: %+v", r)
	}
	if rowOf(t, v, 4).HashState != cellAgree {
		t.Fatal("the hash agreed; only the served root differs")
	}
}

func TestJudgeShadowRootDissent(t *testing.T) {
	v := judge(98, []nodeAnswer{
		ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"),
		ok(3, "0xaa", "0xr", "0xs"), ok(4, "0xaa", "0xr", "0xWRONG"),
	}, 1)
	r := rowOf(t, v, 4)
	if r.ShadowState != cellDissent || r.Verdict != verdictShadowDissent {
		t.Fatalf("shadow dissent: %+v", r)
	}
	if v.ShadowClasses != 2 || v.ShadowAgree != 3 || v.ShadowJudged != 4 {
		t.Fatalf("shadow counts: %+v", v)
	}
}

// besu: no shadow RPC at all. Absence is its contract, never a fault.
func TestJudgeNoIntrospectionIsNotAFault(t *testing.T) {
	quiet := ok(4, "0xaa", "0xr", "")
	quiet.Introspects = false
	v := judge(98, []nodeAnswer{ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"), ok(3, "0xaa", "0xr", "0xs"), quiet}, 1)
	r := rowOf(t, v, 4)
	if r.ShadowState != cellNoIntrospec || r.Verdict != verdictPartial {
		t.Fatalf("opaque client: %+v", r)
	}
	if r.HeaderState != cellAgree {
		t.Fatal("its header root is a plain eth_getBlock read and must still be judged")
	}
	if v.ShadowJudged != 3 {
		t.Fatalf("an unreportable cell was counted: %+v", v)
	}
}

func TestJudgeBehindAndPending(t *testing.T) {
	behind := nodeAnswer{Node: 3, HeadNumber: 95, Status: "ok", Introspects: true} // no answer at 98
	pending := ok(4, "0xaa", "0xr", "")                                            // introspects, shadow not recorded yet
	v := judge(98, []nodeAnswer{ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs"), behind, pending}, 1)
	b := rowOf(t, v, 3)
	if b.HashState != cellBehind || b.Ahead != -3 || b.Verdict != verdictAbsent {
		t.Fatalf("behind row: %+v", b)
	}
	p := rowOf(t, v, 4)
	if p.ShadowState != cellPending || p.Verdict != verdictPartial {
		t.Fatalf("pending row: %+v", p)
	}
	if v.HashJudged != 3 {
		t.Fatalf("a node without an answer was judged: %+v", v)
	}
}

func TestJudgeIStarDissent(t *testing.T) {
	a, b := ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xaa", "0xr", "0xs")
	a.IStarHash, b.IStarHash = "0xfork", "0xelsewhere"
	v := judge(98, []nodeAnswer{a, b}, 1)
	r := rowOf(t, v, 2)
	if r.IStarState != cellDissent || r.Verdict != verdictIStarDissent {
		t.Fatalf("istar dissent: %+v", r)
	}
	if rowOf(t, v, 1).IStarState != cellAgree {
		t.Fatal("the anchor agrees with itself")
	}
}

// A node on another branch crossed the fork on its own block: that is the
// fork already reported, not a second finding.
func TestJudgeForkAbsorbsIStarMismatch(t *testing.T) {
	a, b := ok(1, "0xaa", "0xr", "0xs"), ok(2, "0xbb", "0xq", "0xt")
	a.IStarHash, b.IStarHash = "0xfork", "0xelsewhere"
	v := judge(98, []nodeAnswer{a, b, ok(3, "0xaa", "0xr", "0xs")}, 1)
	if r := rowOf(t, v, 2); r.Verdict != verdictFork {
		t.Fatalf("verdict: %+v", r)
	}
}

func TestPickHeightSkipsNodesThatCannotKeepUp(t *testing.T) {
	isolated := nodeAnswer{Node: 2, HeadNumber: 40, Status: "ok", Isolated: true}
	stalled := nodeAnswer{Node: 3, HeadNumber: 10, Status: "ok", Phase: "stalled"}
	down := nodeAnswer{Node: 4, HeadNumber: 5, Status: "unreachable"}
	settling := nodeAnswer{Node: 5, HeadNumber: 30, Status: "ok", Settling: "reorg at slot 9"}
	h, ok := pickHeight([]nodeAnswer{{Node: 1, HeadNumber: 100, Status: "ok"}, isolated, stalled, down, settling})
	if !ok || h != 98 {
		t.Fatalf("height: %d %v, want 98 (lowest eligible head minus %d)", h, ok, compareDepth)
	}
	if _, ok := pickHeight([]nodeAnswer{isolated, stalled, down}); ok {
		t.Fatal("no eligible node can still yield a comparison height")
	}
}
