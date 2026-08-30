package main

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
)

// newTestChaos builds a chaos with no live disruptoor/clients: enough to exercise the
// quiesce and scenario-submission handlers, which never touch d, els, or cls before
// a job actually runs.
func newTestChaos() *chaos {
	return &chaos{
		log:          slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		jobs:         make(chan job, 16),
		reorgsBy:     map[string]int{},
		minorityRuns: map[string]int{},
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestQuiesceRejectsNewScenarios(t *testing.T) {
	c := newTestChaos()
	mux := c.mux()

	// Before quiescing, GET /quiesce reports the driver still accepting work.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/quiesce", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /quiesce before quiescing: status %d", rec.Code)
	}
	var before struct {
		Quiesced bool `json:"quiesced"`
		Idle     bool `json:"idle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decoding GET /quiesce body: %v", err)
	}
	if before.Quiesced || !before.Idle {
		t.Fatalf("before quiescing: got %+v, want quiesced=false idle=true", before)
	}

	// POST /quiesce sets the flag and reports it back.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/quiesce", nil))
	if rec.Code != 200 {
		t.Fatalf("POST /quiesce: status %d", rec.Code)
	}
	var after struct {
		Quiesced bool `json:"quiesced"`
		Idle     bool `json:"idle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decoding POST /quiesce body: %v", err)
	}
	if !after.Quiesced || !after.Idle {
		t.Fatalf("after quiescing: got %+v, want quiesced=true idle=true", after)
	}

	// A scenario POST after quiescing must be refused with 409, not queued.
	var name string
	for n := range scenarios {
		name = n
		break
	}
	if name == "" {
		t.Fatal("no scenarios registered to exercise")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/scenario/"+name, nil))
	if rec.Code != 409 {
		t.Fatalf("POST /scenario/%s after quiescing: status %d, want 409", name, rec.Code)
	}
	if c.busy() {
		t.Fatal("scenario POST after quiescing must not have been queued")
	}
}
