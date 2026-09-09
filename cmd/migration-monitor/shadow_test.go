package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// shadowFake answers ShadowRoot from a script and records every hash asked,
// in order, for the caller to assert budget/ordering behaviour against.
type shadowFake struct {
	name        string
	introspects bool
	roots       map[string]string // hash -> root; "" means legal null
	errs        map[string]error  // hash -> error to return instead
	asked       []string
}

func (f *shadowFake) Name() string      { return f.name }
func (f *shadowFake) Introspects() bool { return f.introspects }

func (f *shadowFake) ShadowRoot(_ context.Context, hash string) (string, error) {
	f.asked = append(f.asked, hash)
	if err, ok := f.errs[hash]; ok {
		return "", err
	}
	return f.roots[hash], nil
}

func (f *shadowFake) Progress(context.Context) (json.RawMessage, error) {
	return nil, migmon.ErrNoIntrospection
}
func (f *shadowFake) HeadNumber(context.Context) (uint64, error) { return 0, nil }
func (f *shadowFake) NodeInfo(context.Context) (string, error)   { return "enode://" + f.name, nil }
func (f *shadowFake) AddPeer(context.Context, string) error      { return nil }
func (f *shadowFake) HeaderByNumber(context.Context, uint64) (*migmon.Header, error) {
	return nil, nil
}
func (f *shadowFake) HeaderByTag(context.Context, string) (*migmon.Header, error) { return nil, nil }
func (f *shadowFake) HeaderByHash(context.Context, string) (*migmon.Header, error) {
	return nil, nil
}
func (f *shadowFake) PeerCount(context.Context) (int, error) { return 0, nil }

func TestAgreement(t *testing.T) {
	cases := []struct {
		name     string
		roots    map[string]string
		holders  []int // defaults to expected
		expected []int
		orphaned bool
		state    string
		dissent  []int
	}{
		{
			name:    "pending while nobody holds it (a block still in hysteresis)",
			roots:   map[string]string{},
			holders: []int{},
			state:   "pending",
		},
		{
			name:     "none when holders exist but none runs a shadow, even with old reports",
			roots:    map[string]string{"1": "0xa"},
			holders:  []int{1, 2},
			expected: nil,
			state:    "none",
		},
		{
			name:     "gone",
			roots:    map[string]string{"1": "0xa"},
			expected: []int{1},
			orphaned: true,
			state:    "gone",
		},
		{
			name:     "pending",
			roots:    map[string]string{},
			expected: []int{1, 2},
			state:    "pending",
		},
		{
			name:     "none: holders done, nothing reported",
			roots:    map[string]string{},
			holders:  []int{1},
			expected: nil,
			state:    "none",
		},
		{
			name:     "single",
			roots:    map[string]string{"3": "0xa"},
			expected: []int{3},
			state:    "single",
		},
		{
			name:     "partial",
			roots:    map[string]string{"1": "0xa"},
			expected: []int{1, 2},
			state:    "partial",
		},
		{
			name:     "all",
			roots:    map[string]string{"1": "0xa", "2": "0xa"},
			expected: []int{1, 2},
			state:    "all",
		},
		{
			name:     "split 3 vs 1",
			roots:    map[string]string{"1": "0xa", "2": "0xa", "3": "0xa", "4": "0xb"},
			expected: []int{1, 2, 3, 4},
			state:    "split",
			dissent:  []int{4},
		},
		{
			// 2-vs-2 tie: majority is the root of the lowest node id (1 -> 0xa).
			name:     "split 2 vs 2 tie",
			roots:    map[string]string{"1": "0xa", "2": "0xb", "3": "0xa", "4": "0xb"},
			expected: []int{1, 2, 3, 4},
			state:    "split",
			dissent:  []int{2, 4},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			holders := c.expected
			if c.holders != nil {
				holders = c.holders
			}
			state, dissent := agreement(c.roots, holders, c.expected, c.orphaned)
			if state != c.state {
				t.Fatalf("state = %q, want %q", state, c.state)
			}
			if !reflect.DeepEqual(dissent, c.dissent) && !(len(dissent) == 0 && len(c.dissent) == 0) {
				t.Fatalf("dissent = %v, want %v", dissent, c.dissent)
			}
		})
	}
}

func TestShadowStepBudgetAndOrder(t *testing.T) {
	a := &shadowFake{name: "a", introspects: true, roots: map[string]string{"h5": "0x5", "h6": "0x6", "h7": "0x7"}}
	b := &shadowFake{name: "b", introspects: false} // never asked
	table := newShadowTable([]migmon.Client{a, b}, time.Minute, 100)
	table.track("h5", 5)
	table.track("h6", 6)
	table.track("h7", 7)

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	now := time.Unix(1000, 0)
	table.step(context.Background(), log, now, 7, 2)

	if want := []string{"h7", "h6"}; !reflect.DeepEqual(a.asked, want) {
		t.Fatalf("asked = %v, want %v (newest slot first, budget 2)", a.asked, want)
	}
	if len(b.asked) != 0 {
		t.Fatalf("non-introspecting node was asked: %v", b.asked)
	}
	if got := table.roots("h7")["1"]; got != "0x7" {
		t.Fatalf("roots(h7)[1] = %q, want 0x7", got)
	}
	if _, ok := table.roots("h6")["1"]; !ok {
		t.Fatalf("roots(h6) missing node 1's recorded root")
	}
	if _, ok := table.roots("h5")["1"]; ok {
		t.Fatalf("roots(h5) reported before it was asked (budget exhausted)")
	}
}

func TestShadowStepRetryTiming(t *testing.T) {
	a := &shadowFake{name: "a", introspects: true, roots: map[string]string{"h1": "0xa"}}
	table := newShadowTable([]migmon.Client{a}, time.Minute, 100)
	table.track("h1", 1)

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	base := time.Unix(1000, 0)

	// First ask returns nothing yet (simulate by clearing the script for the
	// first call): use a node whose root is "" until we flip it.
	a.roots["h1"] = ""
	table.step(context.Background(), log, base, 1, 1)
	if len(a.asked) != 1 {
		t.Fatalf("expected one ask, got %d", len(a.asked))
	}
	if _, ok := table.roots("h1")["1"]; ok {
		t.Fatalf("\"\" (legal null) must not be recorded as reported")
	}

	// Immediately stepping again must not re-ask before retryEvery.
	table.step(context.Background(), log, base.Add(10*time.Second), 1, 1)
	if len(a.asked) != 1 {
		t.Fatalf("re-asked before retryEvery elapsed: %v", a.asked)
	}

	// After retryEvery it is re-asked; still null, so the wait doubles: not
	// before 2*retryEvery from that ask.
	table.step(context.Background(), log, base.Add(time.Minute+time.Second), 1, 1)
	if len(a.asked) != 2 {
		t.Fatalf("expected re-ask after retryEvery, got %d asks", len(a.asked))
	}
	table.step(context.Background(), log, base.Add(2*time.Minute+30*time.Second), 1, 1)
	if len(a.asked) != 2 {
		t.Fatalf("second retry came before the doubled wait: %v", a.asked)
	}
	// Past the doubled wait, and now the node has a real root: recorded.
	a.roots["h1"] = "0xa"
	table.step(context.Background(), log, base.Add(3*time.Minute+2*time.Second), 1, 1)
	if len(a.asked) != 3 {
		t.Fatalf("expected the third ask after 2*retryEvery, got %d asks", len(a.asked))
	}
	if got := table.roots("h1")["1"]; got != "0xa" {
		t.Fatalf("roots(h1)[1] = %q, want 0xa", got)
	}

	// A recorded root is never asked again, even long after retryEvery.
	table.step(context.Background(), log, base.Add(10*time.Hour), 1, 1)
	if len(a.asked) != 3 {
		t.Fatalf("re-asked a pair with a recorded root: %v", a.asked)
	}
}

func TestShadowStepWarnsOncePerPair(t *testing.T) {
	boom := errors.New("boom")
	a := &shadowFake{name: "a", introspects: true, errs: map[string]error{"h1": boom}}
	table := newShadowTable([]migmon.Client{a}, 0, 100) // retryEvery 0: every step may re-ask
	table.track("h1", 1)

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	now := time.Unix(1000, 0)
	table.step(context.Background(), log, now, 1, 1)
	table.step(context.Background(), log, now.Add(time.Second), 1, 1)

	if len(a.asked) != 2 {
		t.Fatalf("expected the pair asked again after an error, got %d asks", len(a.asked))
	}
	warnCount := strings.Count(buf.String(), `"kind":"warn"`)
	if warnCount != 1 {
		t.Fatalf("warn count = %d, want exactly 1: %s", warnCount, buf.String())
	}
	if _, ok := table.roots("h1")["1"]; ok {
		t.Fatalf("an errored pair must not be recorded as reported")
	}
}

func TestShadowStepRetainSlotsDrops(t *testing.T) {
	a := &shadowFake{name: "a", introspects: true, roots: map[string]string{"old": "0xold"}}
	table := newShadowTable([]migmon.Client{a}, time.Minute, 10)
	table.track("old", 5)

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	table.step(context.Background(), log, time.Unix(1000, 0), 100, 5) // 5+10 < 100: dropped

	if len(a.asked) != 0 {
		t.Fatalf("asked a block past retainSlots: %v", a.asked)
	}
	if _, ok := table.blocks["old"]; ok {
		t.Fatalf("block past retainSlots was not dropped")
	}
}

func TestShadowTrackIdempotent(t *testing.T) {
	a := &shadowFake{name: "a", introspects: true, roots: map[string]string{"h1": "0xa"}}
	table := newShadowTable([]migmon.Client{a}, time.Minute, 100)
	table.track("h1", 1)

	var buf bytes.Buffer
	log := migmon.NewLog(&buf)
	table.step(context.Background(), log, time.Unix(1000, 0), 1, 1)
	if got := table.roots("h1")["1"]; got != "0xa" {
		t.Fatalf("roots(h1)[1] = %q, want 0xa", got)
	}

	table.track("h1", 1) // re-track must not reset the recorded answer
	if got := table.roots("h1")["1"]; got != "0xa" {
		t.Fatalf("re-track cleared a recorded answer: %q", got)
	}
}
