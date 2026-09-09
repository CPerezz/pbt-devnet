package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// chainClient is an execution client serving one linear chain, with an
// optional stall on the head read and a scripted shadow-root answer.
type chainClient struct {
	shadowFake
	blocks   []*migmon.Header // index = number
	head     int
	stall    time.Duration
	shadowOK bool
}

func (c *chainClient) HeaderByTag(ctx context.Context, tag string) (*migmon.Header, error) {
	if c.stall > 0 {
		select {
		case <-time.After(c.stall):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if tag == "finalized" {
		return c.blocks[0], nil
	}
	return c.blocks[c.head], nil
}

func (c *chainClient) HeaderByHash(_ context.Context, hash string) (*migmon.Header, error) {
	for _, b := range c.blocks {
		if b.Hash == hash {
			return b, nil
		}
	}
	return nil, nil
}

// ShadowRoot answers like geth: an error when the tree is broken, null ("")
// for a block the node does not hold, else a root.
func (c *chainClient) ShadowRoot(_ context.Context, hash string) (string, error) {
	c.asked = append(c.asked, hash)
	if !c.shadowOK {
		return "", errors.New("shadow tree not built")
	}
	if h, _ := c.HeaderByHash(context.Background(), hash); h == nil {
		return "", nil
	}
	return "0xshadow-" + hash, nil
}

func (c *chainClient) PeerCount(context.Context) (int, error) { return 3, nil }

func newChainCollector(t *testing.T, genesis uint64, clients ...*chainClient) (*collector, *migmon.Log, *strings.Builder) {
	t.Helper()
	var states []*nodeState
	for i, c := range clients {
		ns := newNodeState(c.Name(), "http://127.0.0.1:1", genesis+1000)
		ns.rpc = c
		_ = i
		states = append(states, ns)
	}
	col := newCollector(states, nil, nil, nil, classifier(nil), 1, genesis, 6, genesis+1000, 900)
	var out strings.Builder
	log := migmon.NewLog(io.MultiWriter(&out, col))
	return col, log, &out
}

// Every tick publishes a whole document; a stalled node costs its own budget,
// not the page: the previous document keeps being served meanwhile.
func TestCollectorServesWhileANodeStalls(t *testing.T) {
	genesis := uint64(time.Now().Unix()) - 600
	blocks := []*migmon.Header{{Hash: "g", Number: 0, Time: genesis}}
	for i := 1; i <= 3; i++ {
		blocks = append(blocks, &migmon.Header{Hash: "b" + string(rune('0'+i)), Parent: blocks[i-1].Hash, Number: uint64(i), Time: genesis + uint64(6*i)})
	}
	fast := &chainClient{shadowFake: shadowFake{name: "el-1-geth-lighthouse", introspects: true}, blocks: blocks, head: 3, shadowOK: true}
	slow := &chainClient{shadowFake: shadowFake{name: "el-2-geth-lighthouse", introspects: true}, blocks: blocks, head: 3, shadowOK: true, stall: time.Hour}
	col, log, _ := newChainCollector(t, genesis, fast, slow)
	if col.document() != nil {
		t.Fatal("a document before the first tick")
	}
	done := make(chan struct{})
	start := time.Now()
	go func() { col.tick(context.Background(), log); close(done) }()
	select {
	case <-done:
	case <-time.After(nodeTimeout + 5*time.Second):
		t.Fatal("tick did not return within the per-node budget")
	}
	if time.Since(start) < nodeTimeout {
		t.Fatalf("tick returned in %s: the stalled node was not given its budget", time.Since(start))
	}
	var s apiState
	if err := json.Unmarshal(col.document(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Seq != 1 || s.Nodes[0].HeadNumber != 3 || s.Nodes[1].Head != "" {
		t.Fatalf("document = seq %d heads %q/%q, want seq 1 with only the fast node's head", s.Seq, s.Nodes[0].Head, s.Nodes[1].Head)
	}
}

// The document end to end: a block only one node holds is pending (not
// "none"), shadow errors reach the alerts once per node, a warning from the
// monitor itself is never explained by a partition, and the tee never blocks
// the tick that emits it.
func TestCollectorDocument(t *testing.T) {
	genesis := uint64(time.Now().Unix()) - 600
	mk := func(names ...string) []*migmon.Header {
		out := []*migmon.Header{{Hash: "g", Number: 0, Time: genesis}}
		for i, n := range names {
			out = append(out, &migmon.Header{Hash: n, Parent: out[i].Hash, Number: uint64(i + 1), Time: genesis + uint64(6*(i+1))})
		}
		return out
	}
	anchorChain := mk("a1", "a2", "a3")
	// Node 2 forked at a2: its x3 is a sibling of the anchor's a3, still in
	// hysteresis, and its shadow tree errors on every block.
	forked := append(append([]*migmon.Header{}, anchorChain[:3]...), &migmon.Header{Hash: "x3", Parent: "a2", Number: 3, Time: genesis + 18})
	n1 := &chainClient{shadowFake: shadowFake{name: "el-1-geth-lighthouse", introspects: true}, blocks: anchorChain, head: 3, shadowOK: true}
	n2 := &chainClient{shadowFake: shadowFake{name: "el-2-geth-lighthouse", introspects: true}, blocks: forked, head: 3, shadowOK: false}
	col, log, out := newChainCollector(t, genesis, n1, n2)
	decode := func() (apiState, map[string]blockView) {
		var s apiState
		if err := json.Unmarshal(col.document(), &s); err != nil {
			t.Fatal(err)
		}
		byHash := map[string]blockView{}
		for _, b := range s.Blocks {
			byHash[b.Hash] = b
		}
		return s, byHash
	}
	col.tick(context.Background(), log)
	if _, blocks := decode(); blocks["x3"].Agreement != "pending" || blocks["x3"].Segment != "" {
		t.Fatalf("x3 (node 2's fresh fork, still in hysteresis) = %+v, want pending, segment-less", blocks["x3"])
	}
	for i := 0; i < 3; i++ {
		col.tick(context.Background(), log)
	}
	log.Emit(migmon.Event{Kind: migmon.EvCritical, Node: "monitor", Detail: "self"})
	col.tick(context.Background(), log)
	s, byHash := decode()
	if x3 := byHash["x3"]; x3.Segment != "s1" || x3.Agreement != "pending" || len(x3.ShadowRoots) != 0 {
		t.Fatalf("x3 after three ticks = %+v, want branch s1, pending: its only holder's tree errors and the anchor never held it", x3)
	}
	if a2 := byHash["a2"]; a2.Agreement != "single" || len(a2.ShadowRoots) != 1 {
		t.Fatalf("a2 = %+v, want single: only the anchor's head is on the canonical chain and it reported", a2)
	}
	warns := strings.Count(out.String(), `"shadow root: shadow tree not built"`)
	if warns != 1 {
		t.Fatalf("shadow warnings = %d, want exactly one for node 2 however many blocks failed", warns)
	}
	self := -1
	for i, a := range s.Alerts {
		if a.Detail == "self" {
			self = i
		}
	}
	if self < 0 || s.Alerts[self].Node != -1 || s.Alerts[self].Expected {
		t.Fatalf("monitor's own critical = %+v, want node -1, not expected", s.Alerts)
	}
	if s.Nodes[0].CursorNumber != 0 || s.Nodes[0].Lag != 0 {
		t.Fatalf("node 1 without a follower cursor = %+v, want no cursor and no lag", s.Nodes[0])
	}
}
