package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// truncate shortens s to at most n runes, marking the cut with "...", so
// a long RPC error or evidence string cannot blow the summary's line
// budget on its own.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func formatTimeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("15:04:05")
}

// nodeForkHashes is one node's provisional and latest fork-block hash, and whether it finalized.
type nodeForkHashes struct {
	provisional string
	latest      string
	finalized   bool
}

// forkHashesByNode tracks each node's first istar hash (provisional) and
// latest hash, updated by istar-reorged and settled by istar-final.
func (v *verifier) forkHashesByNode() map[string]nodeForkHashes {
	out := map[string]nodeForkHashes{}
	for _, ev := range v.monitor {
		h := out[ev.Node]
		switch ev.Kind {
		case migmon.EvIStar:
			if h.provisional == "" {
				h.provisional = ev.Hash
			}
			h.latest = ev.Hash
		case migmon.EvIStarReorged:
			h.latest = ev.Hash
		case migmon.EvIStarFinal:
			h.finalized = true
		default:
			continue
		}
		out[ev.Node] = h
	}
	return out
}

// nodeStageTimes returns node's first parked+Merkle-active time and done time, zero if never reached.
func (v *verifier) nodeStageTimes(node string) (stage2, stage3 time.Time) {
	var events []migmon.Event
	for _, ev := range v.monitor {
		if ev.Kind == migmon.EvProgress && ev.Node == node {
			events = append(events, ev)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	for _, ev := range events {
		if len(ev.Raw) == 0 {
			continue
		}
		p, err := migmon.DecodeProgress(ev.Raw)
		if err != nil {
			continue
		}
		switch progressStage(p) {
		case 2:
			if stage2.IsZero() {
				stage2 = ev.Time
			}
		case 3:
			if stage3.IsZero() {
				stage3 = ev.Time
			}
		}
	}
	return stage2, stage3
}

// renderSummary builds the --summary markdown artifact: schedule, nodes, and check verdicts.
func (v *verifier) renderSummary(results []checkResult) string {
	var b strings.Builder
	dump, schedErr := v.scheduleDump()

	b.WriteString("# Migration devnet run\n\n")
	if schedErr == nil {
		fmt.Fprintf(&b, "- profile: %s\n", dump.Profile)
		fmt.Fprintf(&b, "- fork time: %s\n", time.Unix(dump.Fork, 0).UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "- anchor: node-%d\n", dump.Anchor)
	} else {
		fmt.Fprintf(&b, "- schedule: unavailable (%v)\n", schedErr)
	}
	fmt.Fprintf(&b, "- binary-trie time T: %s\n\n", time.Unix(int64(v.T), 0).UTC().Format(time.RFC3339))

	if schedErr == nil {
		admitted := dump.Admitted()
		matched, _ := v.attributeWindows(dump, v.chaosWindows())
		byOp := map[string][]chaosWindow{} // one window per victim
		for _, ow := range matched {
			byOp[ow.op.Name] = append(byOp[ow.op.Name], ow.window)
		}
		b.WriteString("## Ops\n\n")
		b.WriteString("| op | class | victims | window | observed drop |\n|---|---|---|---|---|\n")
		for _, o := range admitted {
			window := fmt.Sprintf("%s..%s",
				time.Unix(o.Start, 0).UTC().Format("15:04:05"), time.Unix(o.End, 0).UTC().Format("15:04:05"))
			drop := "not isolated"
			if ws := byOp[o.Name]; len(ws) > 0 {
				parts := make([]string, 0, len(ws))
				for _, w := range ws {
					if ev := v.matchReorg(w); ev.matched {
						parts = append(parts, fmt.Sprintf("%s:%d", w.node, ev.depth))
					} else {
						parts = append(parts, w.node+":none")
					}
				}
				drop = strings.Join(parts, ", ")
			}
			fmt.Fprintf(&b, "| %s | %s | %v | %s | %s |\n", o.Name, o.Class, o.Victims, window, drop)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Nodes\n\n")
	b.WriteString("| node | parked+active | done | provisional I* | latest I* |\n|---|---|---|---|---|\n")
	hashes := v.forkHashesByNode()
	for _, node := range v.progressNodes() {
		stage2, stage3 := v.nodeStageTimes(node)
		h := hashes[node]
		latest := truncate(h.latest, 12)
		if h.finalized {
			latest += " (final)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			node, formatTimeOrDash(stage2), formatTimeOrDash(stage3), truncate(h.provisional, 12), latest)
	}
	b.WriteString("\n")

	b.WriteString("## Checks\n\n")
	for _, r := range results {
		fmt.Fprintf(&b, "- %s %s: %s\n", r.verdict, r.id, truncate(r.evidence, 160))
	}
	return b.String()
}
