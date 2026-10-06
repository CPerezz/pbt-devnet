package main

import (
	"strings"
	"testing"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

func testNames() []string {
	return []string{"el-1-geth-lighthouse", "el-2-erigon-lighthouse", "el-3-geth-lighthouse",
		"el-4-besu-lighthouse", "el-5-nethermind-lighthouse", "el-6-geth-lighthouse"}
}

const testOffline = `{"enabled":true,"export_after_seconds":1080,"margin_seconds":300,
 "producers":[{"node":6,"kind":"geth-convert","consumers":[5]},{"node":2,"kind":"erigon-export","consumers":[3]}],
 "consumers":[{"node":5,"expected_seconds":240,"timeout_seconds":900},{"node":3,"expected_seconds":240,"timeout_seconds":900}],
 "online_only":[{"node":1,"reason":"protected anchor: stays on the online path"},{"node":2,"reason":"erigon has no cross-client importer"},{"node":4,"reason":"besu has no importer"}]}`

func newTestOffline(t *testing.T) *offline {
	t.Helper()
	o, err := parseOffline(testOffline, testNames())
	if err != nil {
		t.Fatalf("parseOffline: %v", err)
	}
	return o
}

// Seeding from --offline: the only real merge rule is node 2, where the producers
// loop and the online_only loop both touch one lifecycle and must not
// clobber each other.
func TestParseOfflineSeeds(t *testing.T) {
	o := newTestOffline(t)
	var v nodeView
	o.fill(2, &v) // both producer and online_only
	if v.Importer != "no_importer" || !v.Producer {
		t.Fatalf("node 2 seed = %+v, want no_importer AND producer (both badges)", v)
	}
}

// apply: unknown node or step -> error, no state change, no event. A known
// step updates state, stamps step_since, and emits exactly one EvLifecycle.
func TestOfflineApplyValidation(t *testing.T) {
	cases := []struct {
		name    string
		node    int
		step    string
		wantErr bool
	}{
		{"unknown node", 99, "importing", true},
		{"node below range", 0, "importing", true},
		{"unknown step", 5, "bogus", true},
		{"good producer step", 6, "exporting", false},
		{"good consumer step", 5, "disconnecting", false},
		{"derived step also accepted", 5, "replaying", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newTestOffline(t)
			var buf strings.Builder
			log := migmon.NewLog(&buf)
			err := o.apply(log, tc.node, tc.step, 0, "", "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("apply(%d,%q) err=%v, wantErr=%v", tc.node, tc.step, err, tc.wantErr)
			}
			gotEvent := strings.Contains(buf.String(), `"kind":"lifecycle"`)
			if gotEvent == tc.wantErr {
				t.Fatalf("apply(%d,%q): event emitted=%v, want emitted=%v (buf=%s)", tc.node, tc.step, gotEvent, !tc.wantErr, buf.String())
			}
			if !tc.wantErr {
				var v nodeView
				o.fill(tc.node, &v)
				if v.StepSince == 0 {
					t.Fatalf("accepted step did not stamp step_since: %+v", v)
				}
			}
		})
	}
}

// A producer's exported step with an anchor adds an anchor mark.
func TestOfflineExportedAddsAnchorMark(t *testing.T) {
	o := newTestOffline(t)
	var buf strings.Builder
	log := migmon.NewLog(&buf)
	if err := o.apply(log, 6, "exported", 812, "0xabc", ""); err != nil {
		t.Fatalf("apply exported: %v", err)
	}
	anchors, _ := o.views()
	if len(anchors) != 1 || anchors[0].Number != 812 || anchors[0].Hash != "0xabc" {
		t.Fatalf("anchors = %+v, want one mark at 812/0xabc", anchors)
	}
}

// quiet: a producer mid-export is quiet; once exported it is not. A nil
// offline (feature off) is never quiet.
func TestOfflineQuiet(t *testing.T) {
	o := newTestOffline(t)
	var buf strings.Builder
	log := migmon.NewLog(&buf)
	o.apply(log, 6, "exporting", 0, "", "")
	if !o.quiet(6) {
		t.Fatal("exporting producer must be quiet")
	}
	o.apply(log, 6, "exported", 812, "0xabc", "")
	if o.quiet(6) {
		t.Fatal("exported producer must not stay quiet")
	}
	// A nil offline (feature off) is never quiet.
	var off *offline
	if off.quiet(5) {
		t.Fatal("nil offline must never be quiet")
	}
}

// derive: once reconnected, lag > 2 reads replaying, <= 2 reads caught_up,
// each transition emits exactly one EvLifecycle. A pending node (no swap
// activity yet) is untouched regardless of its progress reading.
func TestOfflineDerive(t *testing.T) {
	o := newTestOffline(t)
	var buf strings.Builder
	log := migmon.NewLog(&buf)
	o.apply(log, 6, "exported", 100, "0xanchor", "")
	o.apply(log, 5, "reconnecting", 0, "", "")
	o.takeReset(5) // pollOnce consumes this before calling derive; simulate that here
	buf.Reset()

	ns := newNodeState("el-5-nethermind-lighthouse", "http://127.0.0.1:1", 0)
	ns.haveHead, ns.lastHead = true, 110
	ns.haveProgress = true
	ns.lastProgress = migmon.MigrationProgress{Binary: &migmon.DirectionProgress{Phase: migmon.DirFollowing, Cursor: 105}} // lag 5 > 2

	o.derive(log, 5, ns, 110)
	var v nodeView
	o.fill(5, &v)
	if v.Importer != "replaying" {
		t.Fatalf("lag>2 after reconnect = %+v, want replaying from the anchor", v)
	}
	if strings.Count(buf.String(), `"phase":"replaying"`) != 1 {
		t.Fatalf("want exactly one replaying event, got %s", buf.String())
	}

	// The 2026-10-05 lap 4 shape: right after the restart the node's own head is as
	// stale as its cursor while the chain tip is 7 ahead. Still replaying, not caught up.
	buf.Reset()
	ns.lastHead, ns.lastProgress.Binary.Cursor = 109, 109
	o.derive(log, 5, ns, 116)
	o.fill(5, &v)
	if v.Importer != "replaying" {
		t.Fatalf("cursor at its own stale head, 7 behind the tip = %+v, want replaying", v)
	}

	buf.Reset()
	o.derive(log, 5, ns, 110) // lag 1 to the tip
	o.fill(5, &v)
	if v.Importer != "caught_up" {
		t.Fatalf("lag<=2 = %+v, want caught_up", v)
	}
	if strings.Count(buf.String(), `"phase":"caught_up"`) != 1 {
		t.Fatalf("want exactly one caught_up event, got %s", buf.String())
	}

	// A seeded-pending node must never be moved by derive, however its
	// progress reads, and must raise nothing.
	buf.Reset()
	ns3 := newNodeState("el-3-geth-lighthouse", "http://127.0.0.1:1", 0)
	ns3.haveHead, ns3.lastHead = true, 500
	ns3.haveProgress = true
	ns3.lastProgress = migmon.MigrationProgress{Binary: &migmon.DirectionProgress{Phase: migmon.DirFollowing, Cursor: 1}}
	o.derive(log, 3, ns3, 500)
	var v3 nodeView
	o.fill(3, &v3)
	if v3.Importer != "pending" {
		t.Fatalf("pending node moved by derive: %+v", v3)
	}
	if buf.Len() != 0 {
		t.Fatalf("pending node must raise nothing, got %s", buf.String())
	}
}

// takeReset fires exactly once on the transition into reconnecting, not on
// every poll while reconnecting holds, and not on other steps.
func TestOfflineTakeResetOnceOnReconnect(t *testing.T) {
	o := newTestOffline(t)
	var buf strings.Builder
	log := migmon.NewLog(&buf)
	o.apply(log, 5, "importing", 0, "", "")
	if o.takeReset(5) {
		t.Fatal("importing must not arm a reset")
	}
	o.apply(log, 5, "reconnecting", 0, "", "")
	if !o.takeReset(5) {
		t.Fatal("the transition into reconnecting must arm a reset")
	}
	if o.takeReset(5) {
		t.Fatal("takeReset must fire only once per transition")
	}
	// Re-observing the same step again (duplicate push) must not re-arm it.
	o.apply(log, 5, "reconnecting", 0, "", "")
	if o.takeReset(5) {
		t.Fatal("re-observing the same step must not re-arm a reset")
	}
}
