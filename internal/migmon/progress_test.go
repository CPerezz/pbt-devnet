package migmon

import (
	"encoding/json"
	"os"
	"testing"
)

// The pre-fork fixture is a LIVE capture (S1, 2026-08-27): a migration-pre
// node on the pinned tip, freshly imported at anchor 0, answering
// debug_migrationProgress over HTTP. If the fork changes its wire shape,
// this file is the tripwire - recapture, do not hand-edit.
func TestDecodeLiveFixture(t *testing.T) {
	blob, err := os.ReadFile("testdata/progress-prefork.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(blob, &envelope); err != nil {
		t.Fatalf("fixture is not a JSON-RPC envelope: %v", err)
	}
	p, err := DecodeProgress(envelope.Result)
	if err != nil {
		t.Fatalf("live payload failed to decode: %v", err)
	}
	if p.Phase != PhaseRunning {
		t.Fatalf("phase %q, want %q", p.Phase, PhaseRunning)
	}
	if !Active(p.Binary) || p.Binary.Phase != DirSynced {
		t.Fatalf("binary direction %+v, want active synced", p.Binary)
	}
	if p.Merkle != nil {
		t.Fatalf("pre-fork payload grew a merkle direction: %+v", p.Merkle)
	}
	if p.Binary.Cursor != 0 || p.Binary.ShadowRoot == "" || p.Binary.CursorHash == "" {
		t.Fatalf("binary direction lost its identity fields: %+v", p.Binary)
	}
}

// Cursor tolerance: plain numbers today, hexutil tomorrow. A
// monitor that dies on "0x2a" would blame the wrong side of the wire.
func TestFlexCursor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{`{"Phase":"following","Cursor":42}`, 42},
		{`{"Phase":"following","Cursor":"0x2a"}`, 42},
		{`{"Phase":"following","Cursor":null}`, 0},
	} {
		var d DirectionProgress
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if uint64(d.Cursor) != tc.want {
			t.Fatalf("%s: cursor %d, want %d", tc.in, d.Cursor, tc.want)
		}
	}
	var d DirectionProgress
	if err := json.Unmarshal([]byte(`{"Cursor":"0xzz"}`), &d); err == nil {
		t.Fatal("garbage hex cursor decoded silently")
	}
}

// A payload with no phase at all must refuse to decode; the monitor would
// otherwise run blind against a node speaking a different dialect.
func TestDecodeRefusesShapeless(t *testing.T) {
	if _, err := DecodeProgress([]byte(`{}`)); err == nil {
		t.Fatal("shapeless payload decoded; the monitor would run blind")
	}
	if _, err := DecodeProgress([]byte(`{"Phase":"done"}`)); err != nil {
		t.Fatalf("terminal payload failed to decode: %v", err)
	}
}
