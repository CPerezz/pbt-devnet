package migmon

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// NodeSample is one node's answer at a sampled height: canonical block hash
// and shadow state root ("" = legal null, not yet written).
type NodeSample struct {
	Node string
	Hash string
	Root string
}

// EvaluateSample runs root-mismatch and hash-split checks over one height's
// answers across nodes. Nodes disagreeing on canonical hash are reported as
// hash-split, not root-mismatch. postIStar downgrades root-mismatch to a warning.
func EvaluateSample(samples []NodeSample, postIStar bool) []Event {
	groups := make(map[string][]NodeSample)
	for _, s := range samples {
		if s.Hash == "" {
			continue // failed fetch for this node; nothing to compare
		}
		groups[s.Hash] = append(groups[s.Hash], s)
	}
	var evs []Event

	if len(groups) > 1 {
		hashes := make([]string, 0, len(groups))
		for h := range groups {
			hashes = append(hashes, h)
		}
		sort.Strings(hashes)
		var parts []string
		for _, h := range hashes {
			names := make([]string, len(groups[h]))
			for i, s := range groups[h] {
				names[i] = s.Node
			}
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("%s=%s", h, strings.Join(names, ",")))
		}
		evs = append(evs, Event{Kind: EvWarn, Detail: "hash-split: " + strings.Join(parts, " vs ")})
	}

	hashes := make([]string, 0, len(groups))
	for h := range groups {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		roots := make(map[string][]string)
		for _, s := range groups[hash] {
			if s.Root == "" {
				continue // legal null, not a value to compare
			}
			roots[s.Root] = append(roots[s.Root], s.Node)
		}
		if len(roots) < 2 {
			continue
		}
		rootKeys := make([]string, 0, len(roots))
		for r := range roots {
			rootKeys = append(rootKeys, r)
		}
		sort.Strings(rootKeys)
		var parts []string
		for _, r := range rootKeys {
			sort.Strings(roots[r])
			parts = append(parts, fmt.Sprintf("%s=%s", strings.Join(roots[r], ","), r))
		}
		kind := EvCritical
		if postIStar {
			kind = EvWarn
		}
		evs = append(evs, Event{Kind: kind, Finding: FindingRootMismatch, Hash: hash, Detail: strings.Join(parts, " vs ")})
	}
	return evs
}

// NullTracker watches one node's shadow-root nulls while it reports active
// (binary or merkle following|synced); the streak resets when the node goes inactive.
type NullTracker struct {
	node        string
	active      bool
	lastNonNull time.Time
	warnFired   bool
	critFired   bool
}

func NewNullTracker(node string) *NullTracker { return &NullTracker{node: node} }

// Observe records one sample's null/non-null outcome at time now.
func (n *NullTracker) Observe(now time.Time, active bool, null bool) []Event {
	if !active {
		n.active = false
		n.warnFired, n.critFired = false, false
		return nil
	}
	if !n.active {
		n.active = true
		n.lastNonNull = now
	}
	if !null {
		n.lastNonNull = now
		n.warnFired, n.critFired = false, false
		return nil
	}

	streak := now.Sub(n.lastNonNull)
	switch {
	case streak > time.Duration(NullCriticalAfterMinutes)*time.Minute && !n.critFired:
		n.critFired, n.warnFired = true, true
		return []Event{{
			Kind: EvCritical, Node: n.node, Finding: FindingNullCritical,
			Detail: fmt.Sprintf("shadow root null for %s while active", streak.Round(time.Second)),
		}}
	case streak > time.Duration(NullWarnAfterMinutes)*time.Minute && !n.warnFired:
		n.warnFired = true
		return []Event{{
			Kind: EvWarn, Node: n.node, Finding: FindingNullWarn,
			Detail: fmt.Sprintf("shadow root null for %s while active", streak.Round(time.Second)),
		}}
	}
	return nil
}

// reorgMemoryCap bounds how many sampled heights ReorgMemory keeps.
const reorgMemoryCap = 512

// ReorgMemory remembers the canonical hash last sampled at each height, per
// node, so a later mismatch there is an observed reorg.
type ReorgMemory struct {
	node  string
	hash  map[uint64]string
	order []uint64 // FIFO of remembered heights, oldest first
}

func NewReorgMemory(node string) *ReorgMemory {
	return &ReorgMemory{node: node, hash: make(map[uint64]string)}
}

// Observe records height's canonical hash as reported now; head is the
// current head, used for reorg depth. A changed hash at a remembered height is a reorg.
func (m *ReorgMemory) Observe(height uint64, hash string, head uint64) []Event {
	if prev, ok := m.hash[height]; ok {
		if prev == hash {
			return nil
		}
		m.hash[height] = hash
		depth := int64(0)
		if head > height {
			depth = int64(head - height)
		}
		return []Event{{
			Kind: EvReorg, Node: m.node, Number: height,
			Detail: fmt.Sprintf("%s -> %s (depth %d)", prev, hash, depth),
		}}
	}
	m.hash[height] = hash
	m.order = append(m.order, height)
	if len(m.order) > reorgMemoryCap {
		delete(m.hash, m.order[0])
		m.order = m.order[1:]
	}
	return nil
}

// Recent returns up to n of the most recently remembered heights, most recent first.
func (m *ReorgMemory) Recent(n int) []uint64 {
	if n > len(m.order) {
		n = len(m.order)
	}
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = m.order[len(m.order)-1-i]
	}
	return out
}
