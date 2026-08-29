package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

func TestVerdict(t *testing.T) {
	cases := []struct {
		name  string
		cells []rootCell
		split bool
		want  string
	}{
		{
			name: "all equal",
			cells: []rootCell{
				{Node: "a", Status: rootValue, Root: "0xsame"},
				{Node: "b", Status: rootValue, Root: "0xsame"},
			},
			want: verdictEqual,
		},
		{
			name: "differing",
			cells: []rootCell{
				{Node: "a", Status: rootValue, Root: "0x1"},
				{Node: "b", Status: rootValue, Root: "0x2"},
			},
			want: verdictDiffer,
		},
		{
			name: "all null - a legal not-yet-recorded state, never a fault",
			cells: []rootCell{
				{Node: "a", Status: rootNull},
				{Node: "b", Status: rootNull},
			},
			want: verdictNotRecorded,
		},
		{
			name: "mixed null - lagging nodes must not read as disagreement",
			cells: []rootCell{
				{Node: "a", Status: rootValue, Root: "0xsame"},
				{Node: "b", Status: rootNull},
			},
			want: verdictPartial,
		},
		{
			name: "canonical split outranks the root comparison",
			cells: []rootCell{
				{Node: "a", Status: rootValue, Root: "0x1"},
				{Node: "b", Status: rootValue, Root: "0x2"},
			},
			split: true,
			want:  verdictSplit,
		},
		{
			name: "no-introspection and unavailable cells carry no opinion",
			cells: []rootCell{
				{Node: "a", Status: rootValue, Root: "0xsame"},
				{Node: "b", Status: rootNoSurface},
				{Node: "c", Status: rootUnavailable},
			},
			want: verdictEqual,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdict(c.cells, c.split); got != c.want {
				t.Fatalf("verdict() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSnapshotSampleRingBounds(t *testing.T) {
	s := newSnapshot(1000)
	ns := []*nodeState{newNodeState("el-1-geth-lighthouse", "http://x", 1000)}
	now := time.Unix(2000, 0)
	for i := range maxRows + 5 {
		s.addSample(now, uint64(i), ns, nil, false)
	}
	view := s.view(now)
	if len(view.Samples) != maxRows {
		t.Fatalf("samples ring holds %d rows, want %d", len(view.Samples), maxRows)
	}
	// The ring drops the oldest first: after maxRows+5 inserts of heights
	// 0..maxRows+4, the surviving window starts at height 5.
	if first := view.Samples[0].Height; first != 5 {
		t.Fatalf("oldest surviving sample height = %d, want 5 (the ring kept a stale row)", first)
	}
	if last := view.Samples[len(view.Samples)-1].Height; last != maxRows+4 {
		t.Fatalf("newest sample height = %d, want %d", last, maxRows+4)
	}
}

func TestSnapshotEventRingBounds(t *testing.T) {
	s := newSnapshot(1000)
	for i := range maxRows + 5 {
		ev := migmon.Event{Time: time.Unix(int64(i), 0), Kind: migmon.EvWarn, Node: "a", Number: uint64(i)}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := s.Write(append(b, '\n')); err != nil {
			t.Fatalf("snapshot.Write: %v", err)
		}
	}
	view := s.view(time.Now())
	if len(view.Events) != maxRows {
		t.Fatalf("events ring holds %d rows, want %d", len(view.Events), maxRows)
	}
	if first := view.Events[0].Number; first != 5 {
		t.Fatalf("oldest surviving event Number = %d, want 5 (the ring kept a stale row)", first)
	}
}

// A non-notable kind (progress, head, sample) must not push a notable event
// out of the ring: every poll and sample tick emits one, and the event list
// exists precisely to filter that noise out.
func TestSnapshotWriteFiltersNonNotableKinds(t *testing.T) {
	s := newSnapshot(1000)
	write := func(kind string) {
		ev := migmon.Event{Time: time.Now(), Kind: kind, Node: "a"}
		b, _ := json.Marshal(ev)
		s.Write(append(b, '\n'))
	}
	write(migmon.EvProgress)
	write(migmon.EvHead)
	write(migmon.EvSample)
	write(migmon.EvCritical)

	view := s.view(time.Now())
	if len(view.Events) != 1 || view.Events[0].Kind != migmon.EvCritical {
		t.Fatalf("want exactly the one notable event, got %+v", view.Events)
	}
}

// Write may be called with a chunk boundary mid-line (a MultiWriter tee
// still gets whatever byte count json.Encoder buffered); a partial line
// must be carried, not dropped or double-counted.
func TestSnapshotWriteHandlesSplitChunks(t *testing.T) {
	s := newSnapshot(1000)
	ev := migmon.Event{Time: time.Now(), Kind: migmon.EvCritical, Detail: "split across writes"}
	b, _ := json.Marshal(ev)
	line := append(b, '\n')
	mid := len(line) / 2
	if _, err := s.Write(line[:mid]); err != nil {
		t.Fatalf("write first half: %v", err)
	}
	if _, err := s.Write(line[mid:]); err != nil {
		t.Fatalf("write second half: %v", err)
	}
	view := s.view(time.Now())
	if len(view.Events) != 1 || view.Events[0].Detail != "split across writes" {
		t.Fatalf("split-chunk event lost or mangled: %+v", view.Events)
	}
}

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
