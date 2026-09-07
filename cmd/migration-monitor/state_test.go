package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

func populatedSnapshot() *snapshot {
	s := newSnapshot(uint64(time.Now().Add(time.Minute).Unix()))
	geth := newNodeState("el-1-geth-lighthouse", "http://x", 1000)
	geth.haveHead, geth.lastHead = true, 42
	geth.haveProgress = true
	geth.lastProgress = migmon.MigrationProgress{
		Phase:  migmon.PhaseRunning,
		Binary: &migmon.DirectionProgress{Phase: migmon.DirFollowing, Cursor: 7, ShadowRoot: "0xabc123def456"},
	}
	other := newNodeState("el-2-someclient-lighthouse", "http://y", 1000)
	other.haveHead, other.lastHead = true, 40
	// A second geth node whose sample answered but recorded no shadow root
	// yet: a legal null, distinct both from "no introspection" (other) and
	// from an actual disagreement.
	lagging := newNodeState("el-3-geth-lighthouse", "http://z", 1000)
	lagging.haveHead, lagging.lastHead = true, 41

	s.setNode(geth)
	s.setNode(other)
	s.setNode(lagging)
	s.addSample(time.Now(), 30,
		[]*nodeState{geth, other, lagging},
		[]migmon.NodeSample{
			{Node: "el-1-geth-lighthouse", Hash: "0xh1", Root: "0xabc123def456"},
			{Node: "el-3-geth-lighthouse", Hash: "0xh3", Root: ""},
		},
		false,
	)
	s.Write(mustEventLine(migmon.Event{Time: time.Now(), Kind: migmon.EvCritical, Node: "el-1-geth-lighthouse", Detail: "example finding"}))
	return s
}

func mustEventLine(ev migmon.Event) []byte {
	b, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// discard is an io.Writer that keeps the *migmon.Log the handlers need
// without writing test events into the real JSONL stream.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestAPIStateHandlerReturnsExpectedFields(t *testing.T) {
	s := populatedSnapshot()
	log := migmon.NewLog(discard{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/state", nil)
	apiStateHandler(s, log)(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("/api/state body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	for _, field := range []string{"now", "fork_time", "seconds_to_fork", "nodes", "samples", "events"} {
		if _, ok := doc[field]; !ok {
			t.Fatalf("/api/state is missing top-level field %q; got keys %v", field, keysOf(doc))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// A no-introspection node must render its unavailable surface as such, not
// as an empty or "-" cell that an operator could misread as a healthy
// value. A not-yet-recorded root must likewise read as legal, not broken.
func TestPageHandlerExecutesAndDistinguishesHonestly(t *testing.T) {
	s := populatedSnapshot()
	log := migmon.NewLog(discard{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	pageHandler(s, log)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("page handler status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if body == "" {
		t.Fatal("page handler rendered an empty body")
	}
	if !strings.Contains(body, "no introspection") {
		t.Fatal("a node with no migration introspection did not render as such")
	}
	if !strings.Contains(body, "not recorded") {
		t.Fatal("a legal not-yet-recorded root did not render distinctly from a disagreement")
	}
}
