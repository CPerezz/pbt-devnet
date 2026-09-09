package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

func TestVictimsOf(t *testing.T) {
	cases := []struct {
		name   string
		groups [][]int
		anchor int
		want   []int
	}{
		{"everyone outside the anchor's group", [][]int{{1, 2, 3}, {4}}, 1, []int{4}},
		{"anchor's group is the network even when smaller", [][]int{{1}, {2, 3, 4}}, 1, []int{2, 3, 4}},
		{"four islands", [][]int{{1}, {2}, {3}, {4}}, 1, []int{2, 3, 4}},
		{"anchor elsewhere", [][]int{{2}, {1, 3, 4}}, 2, []int{1, 3, 4}},
		{"no anchor present: largest group is the network", [][]int{{5, 6}, {3, 4, 7}}, 1, []int{5, 6}},
		{"no groups", nil, 1, nil},
		{"one group sorted", [][]int{{5, 2, 3}}, 1, []int{2, 3, 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := victimsOf(c.groups, c.anchor)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("victimsOf(%v) = %v, want %v", c.groups, got, c.want)
			}
		})
	}
}

// disruptoorStub serves /v1/state from a mutable body, or a 500 when down.
type disruptoorStub struct {
	body string
	down bool
}

func newDisruptoorStub() (*disruptoorStub, *disruptoor.Client) {
	s := &disruptoorStub{body: `{"partitions":[]}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(s.body))
	}))
	return s, disruptoor.New(srv.URL, time.Second)
}

func TestPartitionsParse(t *testing.T) {
	stub, cl := newDisruptoorStub()
	stub.body = `{"partitions":[
		{"name":"deep-1","groups":[{"node-index":["1","2"]},{"node-index":["3"]}]},
		{"name":"no-groups"},
		{}
	]}`
	got, err := cl.Partitions()
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0].Name != "deep-1" || fmt.Sprint(got[0].Groups) != "[[1 2] [3]]" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Name != "no-groups" || len(got[1].Groups) != 0 {
		t.Fatalf("got[1] = %+v", got[1])
	}
	if got[2].Name != "" || len(got[2].Groups) != 0 {
		t.Fatalf("got[2] = %+v", got[2])
	}
}

func TestPartitionsParseEmpty(t *testing.T) {
	_, cl := newDisruptoorStub()
	got, err := cl.Partitions()
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want empty", got)
	}
}

func identityClassOf(name string) string { return "scenario:" + name }

func TestPartitionTrackerTransitions(t *testing.T) {
	stub, cl := newDisruptoorStub()
	tr := newPartitionTracker(cl, identityClassOf, 1)

	// slot 10: partition "a" applied.
	stub.body = `{"partitions":[{"name":"a","groups":[{"node-index":[1,2]},{"node-index":[3]}]}]}`
	if err := tr.poll(10); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	recs := tr.list()
	if len(recs) != 1 || recs[0].Name != "a" || recs[0].AppliedSlot != 10 || recs[0].LiftedSlot != 0 {
		t.Fatalf("after apply: %+v", recs)
	}
	if !tr.isolated(3) || tr.isolated(1) {
		t.Fatalf("isolated wrong after apply: %+v", recs)
	}

	// slot 20: still applied, groups unchanged, no new record.
	if err := tr.poll(20); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(tr.list()) != 1 {
		t.Fatalf("still-applied poll created a record: %+v", tr.list())
	}

	// slot 30: down. State must be kept, error returned.
	stub.down = true
	if err := tr.poll(30); err == nil {
		t.Fatalf("poll 3: want error on 500")
	}
	recs = tr.list()
	if len(recs) != 1 || recs[0].LiftedSlot != 0 {
		t.Fatalf("state changed on error: %+v", recs)
	}
	stub.down = false

	// slot 40: lifted.
	stub.body = `{"partitions":[]}`
	if err := tr.poll(40); err != nil {
		t.Fatalf("poll 4: %v", err)
	}
	recs = tr.list()
	if len(recs) != 1 || recs[0].LiftedSlot != 40 {
		t.Fatalf("after lift: %+v", recs)
	}
	if tr.isolated(3) {
		t.Fatalf("isolated true after lift")
	}

	// slot 50: re-applied is a new record, not a mutation of the old one.
	stub.body = `{"partitions":[{"name":"a","groups":[{"node-index":[1,2]},{"node-index":[3]}]}]}`
	if err := tr.poll(50); err != nil {
		t.Fatalf("poll 5: %v", err)
	}
	recs = tr.list()
	if len(recs) != 2 {
		t.Fatalf("re-application did not create a new record: %+v", recs)
	}
	if recs[0].LiftedSlot != 40 || recs[1].AppliedSlot != 50 || recs[1].LiftedSlot != 0 {
		t.Fatalf("re-application records wrong: %+v", recs)
	}
}

func TestPartitionTrackerCovered(t *testing.T) {
	_, cl := newDisruptoorStub()
	tr := newPartitionTracker(cl, identityClassOf, 1)
	tr.recs = []partition{
		{Name: "a", Victims: []int{3}, AppliedSlot: 10, LiftedSlot: 20},
	}
	const grace = 5
	cases := []struct {
		slot uint64
		node int
		want bool
	}{
		{7, 0, false},   // before applied-2
		{7, 3, false},   // before applied-2, node given
		{8, 3, true},    // applied-2, inclusive edge
		{15, 3, true},   // inside window
		{20, 3, true},   // at lift
		{25, 3, true},   // lifted+grace, inclusive edge
		{26, 3, false},  // past lifted+grace
		{15, 1, false},  // node not a victim
		{15, 0, true},   // node 0 (no node on the event) matches any partition
		{15, -1, false}, // the monitor's own warnings are never explained by a partition
	}
	for _, c := range cases {
		got := tr.covered(c.slot, c.node, grace)
		if got != c.want {
			t.Errorf("covered(%d, %d, %d) = %v, want %v", c.slot, c.node, grace, got, c.want)
		}
	}
}

func TestPartitionTrackerCoveredStillApplied(t *testing.T) {
	_, cl := newDisruptoorStub()
	tr := newPartitionTracker(cl, identityClassOf, 1)
	tr.recs = []partition{{Name: "a", Victims: []int{3}, AppliedSlot: 10, LiftedSlot: 0}}
	if !tr.covered(1_000_000, 3, 5) {
		t.Fatalf("still-applied window should be open-ended")
	}
	if tr.covered(1_000_000, 4, 5) {
		t.Fatalf("node not a victim should not be covered")
	}
}

func TestClassifierKnownAndFallback(t *testing.T) {
	s := &migsched.Schedule{Ops: []migsched.Op{
		{Name: "straddle-1", Class: migsched.ClassStraddle},
		{Name: "deep-1", Class: migsched.ClassDeep},
	}}
	classOf := classifier(s)
	if got := classOf("straddle-1"); got != "straddle" {
		t.Errorf("known name: got %q, want straddle", got)
	}
	if got := classOf("deep-1"); got != "deep" {
		t.Errorf("known name: got %q, want deep", got)
	}
	if got := classOf("pbt-scenario-3"); got != "scenario" {
		t.Errorf("pbt-* fallback: got %q, want scenario", got)
	}
	if got := classOf("proposer-2"); got != "short" {
		t.Errorf("proposer-* fallback: got %q, want short", got)
	}
	if got := classOf("mystery-op"); got != "scenario" {
		t.Errorf("unknown fallback: got %q, want scenario", got)
	}
}

func TestScheduleView(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	const slotSeconds = 12
	s := &migsched.Schedule{Ops: []migsched.Op{
		{
			Name:    "deep-1",
			Class:   migsched.ClassDeep,
			Victims: []int{2, 3},
			Start:   genesis.Add(120 * time.Second), // exactly slot 10
			End:     genesis.Add(133 * time.Second), // slot 11 (floor)
		},
		{
			Name:  "before-genesis",
			Class: migsched.ClassShort,
			Start: genesis.Add(-time.Hour),
			End:   genesis,
		},
	}}
	got := scheduleView(s, uint64(genesis.Unix()), slotSeconds)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name != "deep-1" || got[0].Class != "deep" || got[0].StartSlot != 10 || got[0].EndSlot != 11 {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if fmt.Sprint(got[0].Victims) != "[2 3]" {
		t.Fatalf("got[0].Victims = %v", got[0].Victims)
	}
	if got[1].StartSlot != 0 || got[1].EndSlot != 0 {
		t.Fatalf("at/before genesis should floor to slot 0: %+v", got[1])
	}
}

func TestNodePhase(t *testing.T) {
	const forkTime = 1000
	dp := func(phase, errStr string, cursor uint64) *migmon.DirectionProgress {
		return &migmon.DirectionProgress{Phase: phase, Cursor: migmon.FlexUint64(cursor), CursorHash: "0xh", Error: errStr}
	}
	cases := []struct {
		name           string
		p              *migmon.MigrationProgress
		headTime       uint64
		wantPhase      string
		wantCursor     uint64
		wantCursorHash string
	}{
		{"nil progress", nil, 500, "unknown", 0, ""},
		{"pre-fork nil binary", &migmon.MigrationProgress{Phase: migmon.PhaseRunning}, 500, "unknown", 0, ""},
		{"pre-fork following", &migmon.MigrationProgress{Binary: dp(migmon.DirFollowing, "", 7)}, 500, "following", 7, "0xh"},
		{"pre-fork synced", &migmon.MigrationProgress{Binary: dp(migmon.DirSynced, "", 8)}, 500, "synced", 8, "0xh"},
		{"pre-fork parked", &migmon.MigrationProgress{Binary: dp(migmon.DirParked, "", 9)}, 500, "parked", 9, "0xh"},
		{"pre-fork stalled", &migmon.MigrationProgress{Binary: dp(migmon.DirStalled, "", 1)}, 500, "stalled", 1, "0xh"},
		{"pre-fork error overrides phase", &migmon.MigrationProgress{Binary: dp(migmon.DirFollowing, "boom", 1)}, 500, "stalled", 1, "0xh"},
		{"pre-fork idle parked", &migmon.MigrationProgress{Binary: dp(migmon.DirIdle, "", 0)}, 500, "parked", 0, "0xh"},
		{"post-fork merkle following window", &migmon.MigrationProgress{Merkle: dp(migmon.DirFollowing, "", 3)}, 1500, "window", 3, "0xh"},
		{"post-fork merkle synced window", &migmon.MigrationProgress{Merkle: dp(migmon.DirSynced, "", 4)}, 1500, "window", 4, "0xh"},
		{"post-fork merkle stalled", &migmon.MigrationProgress{Merkle: dp(migmon.DirStalled, "", 5)}, 1500, "stalled", 5, "0xh"},
		{"post-fork merkle error", &migmon.MigrationProgress{Merkle: dp(migmon.DirFollowing, "oops", 6)}, 1500, "stalled", 6, "0xh"},
		{"post-fork merkle parked", &migmon.MigrationProgress{Merkle: dp(migmon.DirParked, "", 9)}, 1500, "parked", 9, "0xh"},
		// geth reports completion at the top level with no directions at all.
		{"post-fork done no directions", &migmon.MigrationProgress{Phase: migmon.PhaseDone}, 1500, "done", 0, ""},
		{"post-fork merkle nil running", &migmon.MigrationProgress{Phase: migmon.PhaseRunning, Binary: dp(migmon.DirFollowing, "", 2)}, 1500, "window", 2, "0xh"},
		{"done wins whatever a direction still says", &migmon.MigrationProgress{Phase: migmon.PhaseDone, Binary: dp(migmon.DirFollowing, "", 2)}, 1500, "done", 0, ""},
		{"post-fork merkle nil unknown no binary", &migmon.MigrationProgress{Phase: migmon.PhaseInactive}, 1500, "unknown", 0, ""},
		{"exactly at fork is post-fork", &migmon.MigrationProgress{Phase: migmon.PhaseRunning}, forkTime, "window", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phase, cursor, hash := nodePhase(c.p, forkTime, c.headTime)
			if phase != c.wantPhase || cursor != c.wantCursor || hash != c.wantCursorHash {
				t.Fatalf("nodePhase() = (%q, %d, %q), want (%q, %d, %q)",
					phase, cursor, hash, c.wantPhase, c.wantCursor, c.wantCursorHash)
			}
		})
	}
}
