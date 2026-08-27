package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// The worked example on the stake-weighted schedule: genesis=1000000,
// T=1002400, deadline T-300=1002100. Deeps pin the heavy victim
// (eligible[0]=2); shorts rotate over the light nodes 3,4. Every window
// ends by +1450 < +2100, so everything is admitted and the forced heal
// fires at the last end.
func TestResolveR3(t *testing.T) {
	s, err := resolve("r3", time.Unix(1000000, 0), time.Unix(1002400, 0), []int{2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start, end int64
		victims    []int
		deep       bool
	}{
		{1000240, 1000430, []int{2}, true},
		{1000540, 1000730, []int{2}, true},
		{1000840, 1001030, []int{2}, true},
		{1001090, 1001240, []int{3}, false},
		{1001300, 1001450, []int{4}, false},
	}
	if len(s.ops) != len(want) {
		t.Fatalf("ops = %d, want %d", len(s.ops), len(want))
	}
	for i, w := range want {
		o := s.ops[i]
		if o.refused != "" {
			t.Fatalf("op %d refused: %s", i, o.refused)
		}
		if o.start.Unix() != w.start || o.end.Unix() != w.end || o.deep != w.deep {
			t.Fatalf("op %d = [%d,%d] deep=%v, want [%d,%d] deep=%v",
				i, o.start.Unix(), o.end.Unix(), o.deep, w.start, w.end, w.deep)
		}
		if len(o.victims) != 1 || o.victims[0] != w.victims[0] {
			t.Fatalf("op %d victims %v, want %v (heavy pin or rotation broke)", i, o.victims, w.victims)
		}
	}
	if s.healAll.Unix() != 1001450 {
		t.Fatalf("healAll = %d, want last end 1001450", s.healAll.Unix())
	}
	if s.quiet.Unix() != 1002100 {
		t.Fatalf("quiet = %d, want T-300 = 1002100", s.quiet.Unix())
	}
}

// A fork too close for some windows: the crossing ops must be refused
// whole, never compressed, while ops that still fit stay admitted; the
// forced heal tracks the last ADMITTED end.
func TestAdmissionRefusesCrossingOps(t *testing.T) {
	// T-300 = genesis+900: deep1 (+430) and deep2 (+730) fit; deep3
	// (+1030) and both shorts must go.
	s, err := resolve("r3", time.Unix(1000000, 0), time.Unix(1001200, 0), []int{2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range s.ops[:2] {
		if o.refused != "" {
			t.Fatalf("op %d refused though it ends before the +900 deadline: %s", i, o.refused)
		}
	}
	for i, o := range s.ops[2:] {
		if o.refused == "" {
			t.Fatalf("op %d admitted; it ends at %d, after deadline %d", i+2, o.end.Unix(), 1000900)
		}
	}
	if s.healAll.Unix() != 1000730 {
		t.Fatalf("healAll = %d, want last admitted end 1000730", s.healAll.Unix())
	}
}

// smoke admits its single op when T leaves room, and the victim is the
// first eligible node.
func TestResolveSmoke(t *testing.T) {
	s, err := resolve("smoke", time.Unix(2000, 0), time.Unix(9000, 0), []int{2})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ops) != 1 || s.ops[0].refused != "" {
		t.Fatalf("smoke schedule = %+v", s.ops)
	}
	if got := s.ops[0]; got.start.Unix() != 2360 || got.end.Unix() != 2540 || got.victims[0] != 2 {
		t.Fatalf("smoke op = [%d,%d] victims %v", got.start.Unix(), got.end.Unix(), got.victims)
	}
}

func TestResolveGuards(t *testing.T) {
	if _, err := resolve("nope", time.Unix(0, 0), time.Unix(1, 0), []int{2}); err == nil {
		t.Fatal("unknown profile resolved")
	}
	if _, err := resolve("smoke", time.Unix(0, 0), time.Unix(1, 0), nil); err == nil {
		t.Fatal("empty victim set resolved; the driver would isolate nobody forever")
	}
}

// Dry-run output: kinds in schedule order, one isolate per victim, then
// the forced heal and the pause marker. Timestamps deliberately not
// asserted (they are wall-clock derived inputs, not logic).
func TestEmitPlanShape(t *testing.T) {
	var buf bytes.Buffer
	s, err := resolve("r3", time.Unix(1000000, 0), time.Unix(1002400, 0), []int{2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	emitPlan(migmon.NewLog(&buf), s)
	var kinds []string
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var ev migmon.Event
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"isolate", "isolate", "isolate", "isolate", "isolate", "heal", "pause"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

// others must exclude every victim and keep ascending order; the majority
// group is what keeps the bootnode connected to the surviving side.
func TestOthers(t *testing.T) {
	got := others(4, []int{3, 1})
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("others = %v, want [2 4]", got)
	}
}
