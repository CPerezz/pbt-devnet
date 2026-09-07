package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// fakeClient answers the two surfaces the gate uses, from a script the test
// controls. Progress returns migmon.ErrNoIntrospection when phases is nil,
// which is how a client with no migration surface behaves.
type fakeClient struct {
	name    string
	phases  []string // consumed one per Progress call, last value repeats
	heads   uint64
	hashAt  map[uint64]string
	callsSt *int
	peered  []string
}

func (f *fakeClient) Name() string      { return f.name }
func (f *fakeClient) Introspects() bool { return f.phases != nil }

func (f *fakeClient) Progress(context.Context) (json.RawMessage, error) {
	if f.phases == nil {
		return nil, migmon.ErrNoIntrospection
	}
	p := f.phases[0]
	if len(f.phases) > 1 {
		f.phases = f.phases[1:]
	}
	if f.callsSt != nil {
		*f.callsSt++
	}
	return json.RawMessage(fmt.Sprintf(`{"Phase":%q,"Binary":null,"Merkle":null}`, p)), nil
}

func (f *fakeClient) ShadowRoot(context.Context, string) (string, error) { return "", nil }
func (f *fakeClient) HeadNumber(context.Context) (uint64, error)         { return f.heads, nil }

func (f *fakeClient) HeaderByNumber(_ context.Context, h uint64) (*migmon.Header, error) {
	hash, ok := f.hashAt[h]
	if !ok {
		return nil, fmt.Errorf("no block %d", h)
	}
	return &migmon.Header{Number: h, Hash: hash}, nil
}

func (f *fakeClient) HeaderByTag(context.Context, string) (*migmon.Header, error) {
	return nil, nil
}

func (f *fakeClient) NodeInfo(context.Context) (string, error) {
	return "enode://" + f.name, nil
}

func (f *fakeClient) AddPeer(_ context.Context, enode string) error {
	f.peered = append(f.peered, enode)
	return nil
}

// Completion is judged only from clients that can answer, and the omission
// is reported: a run whose completion was inferred from a subset of nodes
// proved less than a clean pass.
func TestWaitForDoneSkipsClientsWithoutIntrospection(t *testing.T) {
	var buf capture
	log := migmon.NewLog(&buf)
	clients := []migmon.Client{
		&fakeClient{name: "el-1-geth-lighthouse", phases: []string{"done"}},
		&fakeClient{name: "el-2-otherclient-lighthouse"}, // no introspection
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !waitForDone(ctx, log, clients) {
		t.Fatal("completion was not observed although the answering client reported done")
	}
	if !buf.contains("no migration introspection") {
		t.Fatalf("the skipped client was not reported: %s", buf.String())
	}
}

// With no client able to answer, completion is unobservable: say so rather
// than hand over on a guess.
func TestWaitForDoneRefusesWithoutAnyIntrospection(t *testing.T) {
	var buf capture
	log := migmon.NewLog(&buf)
	clients := []migmon.Client{&fakeClient{name: "el-1-otherclient-lighthouse"}}
	if waitForDone(context.Background(), log, clients) {
		t.Fatal("completion was claimed with no client able to report it")
	}
	if !buf.contains(migmon.FindingBoundary) {
		t.Fatalf("no finding recorded: %s", buf.String())
	}
}

// Convergence compares the canonical hash at the shallowest common height,
// so nodes at different heights are not mistaken for a split.
func TestConverged(t *testing.T) {
	ctx := context.Background()
	agree := []migmon.Client{
		&fakeClient{name: "a", heads: 100, hashAt: map[uint64]string{90: "0xsame", 100: "0xa"}},
		&fakeClient{name: "b", heads: 90, hashAt: map[uint64]string{90: "0xsame"}},
	}
	ok, detail, err := converged(ctx, agree)
	if err != nil || !ok {
		t.Fatalf("nodes at different heights on one chain read as split: ok=%v detail=%q err=%v", ok, detail, err)
	}

	split := []migmon.Client{
		&fakeClient{name: "a", heads: 90, hashAt: map[uint64]string{90: "0xone"}},
		&fakeClient{name: "b", heads: 90, hashAt: map[uint64]string{90: "0xtwo"}},
	}
	ok, detail, err = converged(ctx, split)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a genuine split read as converged")
	}
	if detail == "" {
		t.Fatal("a split produced no evidence line")
	}
}

// The watchdog must stay silent while the schedule is holding a partition:
// healing one mid-window would destroy the behaviour under test.
func TestScheduleCoversHeldWindows(t *testing.T) {
	genesis := time.Unix(1_000_000, 0)
	fork := genesis.Add(1800 * time.Second)
	sched, err := migsched.Resolve("composite", genesis, fork, migsched.Topology{
		Heavy: 2, Lights: []int{3, 4}, HeavyShare: 0.4, SecondsPerSlot: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sched.Covers(fork) {
		t.Fatal("the fork instant is not covered although a straddle is held across it")
	}
	if sched.Covers(genesis.Add(1200 * time.Second)) {
		t.Fatal("a gap between windows reads as covered; a stuck partition there would go unhealed")
	}
}

type capture struct{ b []byte }

func (c *capture) Write(p []byte) (int, error) { c.b = append(c.b, p...); return len(p), nil }
func (c *capture) String() string              { return string(c.b) }
func (c *capture) contains(s string) bool      { return strings.Contains(string(c.b), s) }
