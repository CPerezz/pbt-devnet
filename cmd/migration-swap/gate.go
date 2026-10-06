package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// Evidence is swaps.json; mu guards every record because the SIGTERM writer reads mid-run.
type Evidence struct {
	mu        sync.Mutex
	Producers []*migmon.ProducerRecord `json:"producers"`
	Swaps     []*migmon.SwapRecord     `json:"swaps"`
}

func (e *Evidence) update(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f()
}

func (e *Evidence) marshal() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return json.MarshalIndent(e, "", "  ")
}

// newEvidence seeds every record pessimistically, so an interrupted run still reads truthfully.
func newEvidence(cfg migmon.Offline) (*Evidence, error) {
	e := &Evidence{Producers: []*migmon.ProducerRecord{}, Swaps: []*migmon.SwapRecord{}}
	of := map[int]*migmon.ProducerRecord{}
	for _, p := range cfg.Producers {
		if p.Kind != "geth-convert" && p.Kind != "erigon-export" {
			return nil, fmt.Errorf("producer node %d: unknown kind %q", p.Node, p.Kind)
		}
		pr := &migmon.ProducerRecord{Node: p.Node, Kind: p.Kind, Status: "failed", Detail: "not run",
			ShadowRoots: map[string]string{}, Dir: fmt.Sprintf("artifacts/node-%d", p.Node), Consumers: append([]int{}, p.Consumers...)}
		e.Producers = append(e.Producers, pr)
		for _, c := range p.Consumers {
			of[c] = pr
		}
	}
	for _, c := range cfg.Consumers {
		p, ok := of[c.Node]
		if !ok {
			return nil, fmt.Errorf("consumer node %d has no producer", c.Node)
		}
		e.Swaps = append(e.Swaps, &migmon.SwapRecord{Node: c.Node, Producer: p.Node, Status: "skipped", Detail: "not reached",
			EvidenceDir: fmt.Sprintf("artifacts/consumer-node-%d", c.Node)})
	}
	return e, nil
}

// chaosFree is how long from now no admitted partition occupies, capped at horizon.
func chaosFree(d migsched.Dump, now time.Time, horizon time.Duration) int64 {
	gaps := d.Gaps(now, now.Add(horizon))
	if len(gaps) == 0 || gaps[0][0].After(now) {
		return 0
	}
	return int64(gaps[0][1].Sub(now) / time.Second)
}

// gapHorizon caps chaosFree; every window asked for is far shorter.
const gapHorizon = time.Hour

// exportGap is the chaos-free time an export must start inside.
const exportGap = 180

// swapSlack is the chaos-free time a swap needs beyond its expected duration.
const swapSlack = 120

// tooLate reports whether a swap started now could not finish, with margin, before the fork.
func tooLate(now, fork, expected, margin int64) bool {
	return now+expected+margin > fork
}

// exportState is what an export waits on; Partitions < 0 means disruptoor was unreachable.
type exportState struct {
	Now, NotBefore int64
	Partitions     int
	Gap            int64
	Down           []string // validating ELs not answering eth_blockNumber
}

// exportBlocker names the first unmet export condition, "" when every one holds.
func exportBlocker(s exportState) string {
	switch {
	case s.Now < s.NotBefore:
		return fmt.Sprintf("export not before %d", s.NotBefore)
	case s.Partitions < 0:
		return "disruptoor state unknown"
	case s.Partitions > 0:
		return fmt.Sprintf("%d partitions applied", s.Partitions)
	case s.Gap < exportGap:
		return fmt.Sprintf("chaos-free for %ds, need %ds", s.Gap, exportGap)
	case len(s.Down) > 0:
		return "not answering: " + strings.Join(s.Down, ", ")
	}
	return ""
}

type verdict int

const (
	wait verdict = iota
	proceed
	skip    // too late, or stale finality after a failed swap
	reorged // the anchor is not canonical on node 1: the producer's artifacts are dead
)

// swapState is what one swap waits on; Partitions < 0 means disruptoor was unreachable.
type swapState struct {
	Now, Fork, Expected, Margin int64
	Anchor                      uint64
	AnchorHash                  string
	Finalized                   *migmon.Header // node 1's finalized block; nil = unknown
	Canonical                   string         // node 1's hash at the anchor height; "" = unknown
	EpochSeconds                int64
	Down                        []string // other validating ELs/CLs failing their health check
	Partitions                  int
	Gap                         int64
	PrevBehind                  bool // a failed or timed-out previous consumer's EL is off the head
	PrevFailed                  bool // an earlier swap failed or timed out
}

// decideSwap is the swap gate: skip and reorged are final, wait polls again.
func decideSwap(s swapState) (verdict, string) {
	if tooLate(s.Now, s.Fork, s.Expected, s.Margin) {
		return skip, fmt.Sprintf("too late: %d+%d+%d > fork %d", s.Now, s.Expected, s.Margin, s.Fork)
	}
	f := s.Finalized
	if f != nil && f.Number >= s.Anchor && s.Canonical != "" && !strings.EqualFold(s.Canonical, s.AnchorHash) {
		return reorged, fmt.Sprintf("anchor %d is %s on node 1, not %s", s.Anchor, s.Canonical, s.AnchorHash)
	}
	stale := f != nil && s.Now-int64(f.Time) > 4*s.EpochSeconds
	if stale && s.PrevFailed {
		return skip, "a previous swap failed and finality is stale"
	}
	switch {
	case f == nil:
		return wait, "node 1 finality unknown"
	case f.Number < s.Anchor:
		return wait, fmt.Sprintf("anchor %d not finalized (finalized %d)", s.Anchor, f.Number)
	case s.Canonical == "":
		return wait, "anchor block unknown on node 1"
	case stale:
		return wait, fmt.Sprintf("finality stale: finalized block is %ds old", s.Now-int64(f.Time))
	case len(s.Down) > 0:
		return wait, "unhealthy: " + strings.Join(s.Down, ", ")
	case s.Partitions < 0:
		return wait, "disruptoor state unknown"
	case s.Partitions > 0:
		return wait, fmt.Sprintf("%d partitions applied", s.Partitions)
	case s.Gap < s.Expected+swapSlack:
		return wait, fmt.Sprintf("chaos-free for %ds, need %ds", s.Gap, s.Expected+swapSlack)
	case s.PrevBehind:
		return wait, "previous consumer's EL is not back at the head"
	}
	return proceed, ""
}
