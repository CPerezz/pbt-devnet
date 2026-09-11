package migmon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
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

func TestErigonProgress(t *testing.T) {
	fork := hexutil.Uint64(1000)
	armed := erigonMigration{Mode: "hex+bin", ActivationTime: &fork}
	stopped := armed
	stopped.ShadowStopped = true
	pre := &Header{Number: 7, Hash: "0x07", Time: 994}
	post := &Header{Number: 9, Hash: "0x09", Time: 1006}
	for _, tc := range []struct {
		name            string
		m               erigonMigration
		head, finalized *Header
		phase, bin, mer string
	}{
		{"before the fork", armed, pre, nil, PhaseRunning, DirSynced, "nil"},
		{"after the fork", armed, post, pre, PhaseRunning, DirParked, DirSynced},
		{"fork block finalized", armed, post, &Header{Number: 8, Time: 1000}, PhaseDone, "nil", "nil"},
		{"shadow stopped", stopped, pre, nil, PhaseRunning, DirStalled, "nil"},
		{"binary at genesis", erigonMigration{Mode: "bin"}, pre, nil, PhaseInactive, "nil", "nil"},
		{"no activation time", erigonMigration{Mode: "hex+bin"}, post, nil, PhaseInactive, "nil", "nil"},
		{"no head", armed, nil, nil, PhaseInactive, "nil", "nil"},
		{"shadow stopped after the fork", stopped, post, pre, PhaseRunning, DirParked, DirStalled},
	} {
		raw, err := json.Marshal(erigonProgress(tc.m, tc.head, tc.finalized, "0xroot"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := DecodeProgress(raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p.Phase != tc.phase || dirPhase(p.Binary) != tc.bin || dirPhase(p.Merkle) != tc.mer {
			t.Fatalf("%s: got %s binary=%s merkle=%s, want %s binary=%s merkle=%s",
				tc.name, p.Phase, dirPhase(p.Binary), dirPhase(p.Merkle), tc.phase, tc.bin, tc.mer)
		}
		if p.Phase != PhaseRunning {
			continue
		}
		live := p.Binary
		if p.Merkle != nil {
			live = p.Merkle
		}
		if uint64(live.Cursor) != tc.head.Number || live.CursorHash != tc.head.Hash || live.ShadowRoot != "0xroot" {
			t.Fatalf("%s: live direction %+v does not carry the head %+v", tc.name, live, tc.head)
		}
	}
}

func TestErigonClientStaysDoneWhenFinalizedFails(t *testing.T) {
	hash := "0x" + strings.Repeat("11", 32)
	var finalizedCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch {
		case req.Method == "debug_migrationProgress":
			reply["result"] = map[string]any{"mode": "hex+bin", "activationTime": "0x3e8"}
		case req.Method == "debug_shadowStateRoot":
			reply["result"] = hash
		case req.Params[0] == "latest":
			reply["result"] = map[string]any{"number": "0x9", "hash": hash, "timestamp": "0x3ee"}
		case finalizedCalls.Add(1) == 1:
			reply["result"] = map[string]any{"number": "0x8", "hash": hash, "timestamp": "0x3e8"}
		default:
			reply["error"] = map[string]any{"code": -32000, "message": "context deadline exceeded"}
		}
		if err := json.NewEncoder(w).Encode(reply); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	c := NewClient("el-2-erigon-lighthouse", srv.URL)
	for poll := range 2 {
		raw, err := c.Progress(context.Background())
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		p, err := DecodeProgress(raw)
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if p.Phase != PhaseDone {
			t.Fatalf("poll %d: phase %s, want %s", poll, p.Phase, PhaseDone)
		}
	}
}
