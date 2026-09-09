package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// decodeEvents parses one migmon.Event per JSONL line.
func decodeEvents(t *testing.T, buf *bytes.Buffer) []migmon.Event {
	t.Helper()
	var out []migmon.Event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var ev migmon.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decoding event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// A mutual op puts every victim on its own island behind the rest of the
// network; a plain op puts the victims together.
func TestIslands(t *testing.T) {
	mutual := islands(4, migsched.Op{Victims: []int{2, 3, 4}, Mutual: true})
	want := [][]int{{1}, {2}, {3}, {4}}
	if len(mutual) != len(want) {
		t.Fatalf("mutual islands = %v, want %v", mutual, want)
	}
	for i := range want {
		if len(mutual[i]) != 1 || mutual[i][0] != want[i][0] {
			t.Fatalf("mutual islands = %v, want %v", mutual, want)
		}
	}
	pair := islands(4, migsched.Op{Victims: []int{2, 3}})
	if len(pair) != 2 || len(pair[0]) != 2 || pair[0][0] != 1 || pair[0][1] != 4 || len(pair[1]) != 2 {
		t.Fatalf("pair islands = %v, want [[1 4] [2 3]]", pair)
	}
}

// fakeClock advances only when sleep is called, so the hold runs in zero wall time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(d time.Duration) bool {
	c.t = c.t.Add(d)
	return true
}

// countingProbe crosses victim v once it has been polled thresh[v] times; a
// victim in errAlways never returns a clean answer.
func countingProbe(thresh map[int]int, errAlways map[int]bool) crossedFn {
	polls := map[int]int{}
	return func(_ context.Context, v int) (bool, error) {
		if errAlways[v] {
			return false, errors.New("rpc down")
		}
		polls[v]++
		return polls[v] >= thresh[v], nil
	}
}

func TestHoldUntilCrossed(t *testing.T) {
	t.Run("all cross before the deadline", func(t *testing.T) {
		var buf bytes.Buffer
		log := migmon.NewLog(&buf)
		clk := &fakeClock{t: time.Unix(1000, 0)}
		op := migsched.Op{Victims: []int{2, 3, 4}, HoldUntil: clk.t.Add(1000 * time.Second)}
		probe := countingProbe(map[int]int{2: 1, 3: 3, 4: 5}, nil)

		crossed, unreachable, ok := holdUntilCrossed(log, op, probe, 6, clk.now, clk.sleep)
		if !ok || len(crossed) != 3 || len(unreachable) != 0 {
			t.Fatalf("crossed=%v unreachable=%v ok=%v, want all 3 crossed, none unreachable", crossed, unreachable, ok)
		}
		if !clk.t.Before(op.HoldUntil) {
			t.Fatalf("returned at or after HoldUntil; want an early return once every victim crossed")
		}
		events := decodeEvents(t, &buf)
		var pauses, isolates int
		for _, ev := range events {
			switch ev.Kind {
			case migmon.EvPause:
				pauses++
			case migmon.EvIsolate:
				isolates++
			}
		}
		if pauses != 1 {
			t.Fatalf("pause events = %d, want exactly 1", pauses)
		}
		if isolates != 3 {
			t.Fatalf("isolate-crossed events = %d, want 1 per victim (3)", isolates)
		}
	})

	t.Run("one victim never crosses", func(t *testing.T) {
		var buf bytes.Buffer
		log := migmon.NewLog(&buf)
		clk := &fakeClock{t: time.Unix(1000, 0)}
		op := migsched.Op{Victims: []int{2, 3}, HoldUntil: clk.t.Add(18 * time.Second)}
		probe := countingProbe(map[int]int{2: 1, 3: 1_000_000}, nil)

		crossed, unreachable, ok := holdUntilCrossed(log, op, probe, 6, clk.now, clk.sleep)
		if !ok {
			t.Fatalf("ok = false, want a normal return at HoldUntil")
		}
		if !crossed[2] || crossed[3] {
			t.Fatalf("crossed = %v, want only victim 2", crossed)
		}
		if unreachable[3] {
			t.Fatalf("a victim that answered but never crossed must not be reported unreachable")
		}
		if !clk.t.Before(op.HoldUntil.Add(time.Second)) {
			t.Fatalf("clock ran past HoldUntil")
		}
	})

	t.Run("a victim erroring on every poll is unreachable", func(t *testing.T) {
		var buf bytes.Buffer
		log := migmon.NewLog(&buf)
		clk := &fakeClock{t: time.Unix(1000, 0)}
		op := migsched.Op{Victims: []int{2}, HoldUntil: clk.t.Add(18 * time.Second)}
		probe := countingProbe(nil, map[int]bool{2: true})

		crossed, unreachable, ok := holdUntilCrossed(log, op, probe, 6, clk.now, clk.sleep)
		if !ok || len(crossed) != 0 || !unreachable[2] {
			t.Fatalf("crossed=%v unreachable=%v ok=%v, want none crossed and victim 2 unreachable", crossed, unreachable, ok)
		}
		warns := 0
		for _, ev := range decodeEvents(t, &buf) {
			if ev.Kind == migmon.EvWarn {
				warns++
			}
		}
		if warns != 1 {
			t.Fatalf("warn events = %d, want exactly 1 despite polling every slot", warns)
		}
	})
}

// TestUnappliedOpNeverHealed drives execute against a disruptoor that refuses
// every partition: the op must be skipped at its end, never healed or cleared.
func TestUnappliedOpNeverHealed(t *testing.T) {
	var clears int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/state":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/state/clear":
			clears++
		}
	}))
	defer srv.Close()

	start := time.Unix(2000, 0)
	op := migsched.Op{Name: "op1", Class: migsched.ClassShort, Start: start, End: start.Add(10 * time.Second), Victims: []int{2}}
	sched := migsched.Schedule{Profile: "t", Ops: []migsched.Op{op}, Quiet: start.Add(20 * time.Second), SlotSeconds: 6}

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	d := disruptoor.New(srv.URL, 5*time.Second)
	clk := clock{now: func() time.Time { return start }, sleepUntil: func(time.Time) bool { return true }}
	probe := func(context.Context, int) (bool, error) { return false, nil }

	if !execute(log, d, sched, nil, nil, probe, clk) {
		t.Fatal("execute returned false with an always-true sleepUntil")
	}
	if clears != 0 {
		t.Fatalf("clear was called %d times for an op that never applied", clears)
	}
	var heals, skips int
	for _, ev := range decodeEvents(t, &buf) {
		switch {
		case ev.Kind == migmon.EvHeal:
			heals++
		case ev.Kind == migmon.EvSkip && ev.Detail == "never applied: op1":
			skips++
		}
	}
	if heals != 0 {
		t.Fatalf("heal events = %d, want 0", heals)
	}
	if skips != 1 {
		t.Fatalf("skip events with \"never applied\" = %d, want 1 (one victim)", skips)
	}
}
