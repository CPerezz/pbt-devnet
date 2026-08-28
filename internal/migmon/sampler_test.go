package migmon

import (
	"strings"
	"testing"
	"time"
)

// F1/hash-split truth table.
func TestEvaluateSample(t *testing.T) {
	for _, tc := range []struct {
		name       string
		samples    []NodeSample
		postBStar  bool
		wantKind   string // "" = no finding at all
		wantDetail string
	}{
		{
			name: "agreement, no finding",
			samples: []NodeSample{
				{Node: "a", Hash: "0xh", Root: "0xr"},
				{Node: "b", Hash: "0xh", Root: "0xr"},
			},
			wantKind: "",
		},
		{
			name: "same hash, different non-empty roots is critical F1",
			samples: []NodeSample{
				{Node: "a", Hash: "0xh", Root: "0xr1"},
				{Node: "b", Hash: "0xh", Root: "0xr2"},
			},
			wantKind:   EvCritical,
			wantDetail: "a=0xr1",
		},
		{
			name: "one null root never compares",
			samples: []NodeSample{
				{Node: "a", Hash: "0xh", Root: ""},
				{Node: "b", Hash: "0xh", Root: "0xr2"},
			},
			wantKind: "",
		},
		{
			name: "different canonical hashes is hash-split, not F1",
			samples: []NodeSample{
				{Node: "a", Hash: "0xh1", Root: "0xr"},
				{Node: "b", Hash: "0xh2", Root: "0xr"},
			},
			wantKind:   EvWarn,
			wantDetail: "hash-split",
		},
		{
			name: "post-b* root mismatch downgrades to warn",
			samples: []NodeSample{
				{Node: "a", Hash: "0xh", Root: "0xr1"},
				{Node: "b", Hash: "0xh", Root: "0xr2"},
			},
			postBStar: true,
			wantKind:  EvWarn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := EvaluateSample(tc.samples, tc.postBStar)
			if tc.wantKind == "" {
				if len(evs) != 0 {
					t.Fatalf("want no findings, got %+v", evs)
				}
				return
			}
			found := false
			for _, e := range evs {
				if e.Kind == tc.wantKind && strings.Contains(e.Detail, tc.wantDetail) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want kind=%s detail containing %q, got %+v", tc.wantKind, tc.wantDetail, evs)
			}
		})
	}
}

func TestEvaluateSampleF1NeverWaivedPreBStar(t *testing.T) {
	evs := EvaluateSample([]NodeSample{
		{Node: "a", Hash: "0xh", Root: "0xr1"},
		{Node: "b", Hash: "0xh", Root: "0xr2"},
	}, false)
	if len(evs) != 1 || evs[0].Kind != EvCritical || evs[0].Finding != FindingRootMismatch {
		t.Fatalf("F1 must be critical and never waived pre-b*, got %+v", evs)
	}
}

// NULL5 -> NULL10 escalation, and reset on any non-null.
func TestNullTrackerEscalation(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nt := NewNullTracker("n")

	if evs := nt.Observe(base, true, true); len(evs) != 0 {
		t.Fatalf("streak baseline poll must not fire, got %+v", evs)
	}
	if evs := nt.Observe(base.Add(4*time.Minute), true, true); len(evs) != 0 {
		t.Fatalf("under warn threshold must not fire, got %+v", evs)
	}
	evs := nt.Observe(base.Add(6*time.Minute), true, true)
	if !hasFinding(evs, EvWarn, FindingNullWarn) {
		t.Fatalf("past 5min null streak must warn NULL5, got %+v", evs)
	}
	if evs := nt.Observe(base.Add(7*time.Minute), true, true); len(evs) != 0 {
		t.Fatalf("warn must not repeat, got %+v", evs)
	}
	evs = nt.Observe(base.Add(11*time.Minute), true, true)
	if !hasFinding(evs, EvCritical, FindingNullCritical) {
		t.Fatalf("past 10min null streak must critical NULL10, got %+v", evs)
	}
	if evs := nt.Observe(base.Add(20*time.Minute), true, true); len(evs) != 0 {
		t.Fatalf("critical must not repeat, got %+v", evs)
	}

	// Any non-null resets the streak, and the same escalation can re-fire.
	if evs := nt.Observe(base.Add(21*time.Minute), true, false); len(evs) != 0 {
		t.Fatalf("non-null must not itself fire, got %+v", evs)
	}
	if evs := nt.Observe(base.Add(21*time.Minute+30*time.Second), true, true); len(evs) != 0 {
		t.Fatalf("fresh streak under threshold must not fire, got %+v", evs)
	}
	evs = nt.Observe(base.Add(27*time.Minute+30*time.Second), true, true)
	if !hasFinding(evs, EvWarn, FindingNullWarn) {
		t.Fatalf("re-armed streak must warn again after reset, got %+v", evs)
	}
}

func TestNullTrackerInactiveNeverFires(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nt := NewNullTracker("n")
	nt.Observe(base, true, true)
	if evs := nt.Observe(base.Add(30*time.Minute), false, true); len(evs) != 0 {
		t.Fatalf("inactive node must never fire null findings, got %+v", evs)
	}
	// Becoming active again must restart the streak, not resume the old one.
	if evs := nt.Observe(base.Add(31*time.Minute), true, true); len(evs) != 0 {
		t.Fatalf("newly-active baseline poll must not fire, got %+v", evs)
	}
}

func TestReorgMemory(t *testing.T) {
	m := NewReorgMemory("n")
	if evs := m.Observe(100, "0xa", 110); len(evs) != 0 {
		t.Fatalf("first observation at a height must not fire, got %+v", evs)
	}
	if evs := m.Observe(100, "0xa", 112); len(evs) != 0 {
		t.Fatalf("unchanged hash must not fire, got %+v", evs)
	}
	evs := m.Observe(100, "0xb", 112)
	if len(evs) != 1 || evs[0].Kind != EvReorg || evs[0].Number != 100 {
		t.Fatalf("changed hash must fire one reorg at the remembered height, got %+v", evs)
	}
	if !strings.Contains(evs[0].Detail, "0xa -> 0xb") || !strings.Contains(evs[0].Detail, "depth 12") {
		t.Fatalf("reorg detail must name old->new and depth, got %q", evs[0].Detail)
	}
	// Reported once; the memory now holds the new hash.
	if evs := m.Observe(100, "0xb", 115); len(evs) != 0 {
		t.Fatalf("re-observing the settled hash must not refire, got %+v", evs)
	}
}

func TestReorgMemoryBounded(t *testing.T) {
	m := NewReorgMemory("n")
	for h := uint64(0); h < reorgMemoryCap+10; h++ {
		m.Observe(h, "0xhash", h)
	}
	if len(m.order) != reorgMemoryCap {
		t.Fatalf("memory must stay capped at %d, got %d", reorgMemoryCap, len(m.order))
	}
	if _, ok := m.hash[0]; ok {
		t.Fatalf("oldest height must be evicted once the cap is exceeded")
	}
	if _, ok := m.hash[reorgMemoryCap+9]; !ok {
		t.Fatalf("newest height must still be remembered")
	}
	recent := m.Recent(3)
	if len(recent) != 3 || recent[0] != reorgMemoryCap+9 {
		t.Fatalf("Recent must return the newest heights first, got %v", recent)
	}
}

// Frozen-fixture sanity: the live pre-fork payload feeds a Timeline without
// tripping any finding (synced binary, no merkle yet).
func TestTimelineAcceptsLiveFixture(t *testing.T) {
	p := MigrationProgress{
		Phase:  PhaseRunning,
		Binary: &DirectionProgress{Phase: DirSynced, Cursor: 0, CursorHash: "0x660c", ShadowRoot: "0xcf28"},
	}
	tl := NewTimeline("n", 999999999999)
	evs := tl.ObservePoll(p, 1)
	if countKind(evs, EvCritical) != 0 {
		t.Fatalf("live pre-fork fixture must not trip a finding, got %+v", evs)
	}
}
