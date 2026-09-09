package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// lapManifest is the lap driver's own record of what it did: profile launched,
// restart executed, scenarios run and their outcomes, quiesce time.
type lapManifest struct {
	Profile string `json:"profile"`
	Restart *struct {
		Node int   `json:"node"`
		At   int64 `json:"at"` // unix seconds when the stop was issued
	} `json:"restart"`
	Scenarios []struct {
		Name    string `json:"name"`
		Outcome string `json:"outcome"` // pbtchaos's own per-scenario outcome
	} `json:"scenarios"`
	Requested        []string `json:"requested"` // scenario names the lap asked for, regardless of outcome
	HandoverExpected bool     `json:"handover_expected"`
	QuiescedAt       int64    `json:"quiesced_at"` // unix seconds, 0 = never quiesced
}

func loadManifest(path string) (*lapManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m lapManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &m, nil
}

// checkLapManifest reconciles the lap manifest against the run's evidence; every
// recorded expectation is mandatory.
func (v *verifier) checkLapManifest(ctx context.Context) (verdict, string) {
	if v.manifestPath == "" {
		return verdictInconclusive, "no --manifest given (run driven outside the lap script)"
	}
	if v.manifestErr != nil {
		return verdictFail, fmt.Sprintf("manifest: %v", v.manifestErr)
	}
	m := v.manifest
	var problems, notes []string

	dump, err := v.scheduleDump()
	if err != nil {
		problems = append(problems, fmt.Sprintf("schedule: %v", err))
	} else {
		if m.Profile != "" && dump.Profile != m.Profile {
			problems = append(problems, fmt.Sprintf("manifest profile %q != published schedule profile %q", m.Profile, dump.Profile))
		}
		// The restart must land outside every admitted op's window (±60s) and
		// outside a partition's contaminated evidence.
		if m.Restart != nil {
			at := time.Unix(m.Restart.At, 0)
			for _, op := range dump.Admitted() {
				start := time.Unix(op.Start, 0).Add(-60 * time.Second)
				end := time.Unix(op.End, 0).Add(60 * time.Second)
				if !at.Before(start) && !at.After(end) {
					problems = append(problems, fmt.Sprintf("restart of node %d at %s falls inside admitted op %s [%s, %s] (+-60s)",
						m.Restart.Node, at.Format(time.RFC3339), op.Name, start.Format(time.RFC3339), end.Format(time.RFC3339)))
				}
			}
			notes = append(notes, fmt.Sprintf("restart of node %d executed at %s", m.Restart.Node, at.Format(time.RFC3339)))
		}
	}

	if m.HandoverExpected {
		handover := false
		for _, ev := range v.chaos {
			if strings.Contains(ev.Detail, "handing disruptoor over") {
				handover = true
				break
			}
		}
		if !handover {
			problems = append(problems, "manifest expects a gate handover but no handover record appears in the chaos/gate stream")
		}
		if len(m.Scenarios) == 0 {
			problems = append(problems, "manifest expects a handover but records zero scenarios: the post-switchover suite never ran")
		}
	}
	for _, s := range m.Scenarios {
		if s.Outcome != "ok" {
			problems = append(problems, fmt.Sprintf("scenario %s ended %q, want ok", s.Name, s.Outcome))
		}
	}
	for _, name := range m.Requested {
		found := false
		for _, s := range m.Scenarios {
			if s.Name == name {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("requested scenario %s has no recorded outcome", name))
		}
	}
	if n := len(m.Scenarios); n > 0 {
		notes = append(notes, fmt.Sprintf("%d scenario(s) completed ok", n))
	}

	if m.QuiescedAt != 0 {
		q := time.Unix(m.QuiescedAt, 0)
		for _, ev := range v.chaos {
			if ev.Kind == migmon.EvIsolate && ev.Time.After(q.Add(30*time.Second)) {
				problems = append(problems, fmt.Sprintf("isolation at %s after the %s quiesce: end-state evidence was judged under live chaos",
					ev.Time.Format(time.RFC3339), q.Format(time.RFC3339)))
			}
		}
		notes = append(notes, fmt.Sprintf("chaos quiesced at %s", q.Format(time.RFC3339)))
	}

	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	if len(notes) == 0 {
		notes = append(notes, "manifest present, nothing scheduled to reconcile")
	}
	return verdictPass, strings.Join(notes, "; ")
}

// checkInjectedState asserts every node agrees on the injected slots' final
// values (majority wins unless overwritten by a later victim-side salvage),
// and no orphaned injected block stays canonical. INCONCLUSIVE with no
// injection records.
func (v *verifier) checkInjectedState(ctx context.Context) (verdict, string) {
	var injections []migmon.Injection
	for _, ev := range v.chaos {
		if ev.Kind != migmon.EvInject || len(ev.Raw) == 0 {
			continue
		}
		var inj migmon.Injection
		if err := json.Unmarshal(ev.Raw, &inj); err != nil {
			return verdictFail, fmt.Sprintf("undecodable inject record at %s: %v", ev.Time.Format(time.RFC3339), err)
		}
		injections = append(injections, inj)
	}
	if len(injections) == 0 {
		return verdictInconclusive, "no injection records (profile ran without the straddle state injector)"
	}

	var problems, notes []string
	salvaged, evicted := 0, 0
	for _, inj := range injections {
		// Cross-node agreement on the touched slot is the assertion.
		var first string
		firstNode := ""
		agree := true
		for _, e := range v.els {
			got, err := v.getStorageAt(ctx, e, inj.Contract, inj.Slot)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: storage %s[%s]: %v", e.name, inj.Contract, inj.Slot, err))
				continue
			}
			if firstNode == "" {
				first, firstNode = got, e.name
				continue
			}
			if got != first {
				agree = false
				problems = append(problems, fmt.Sprintf("storage %s[%s]: %s says %s, %s says %s",
					inj.Contract, inj.Slot, firstNode, first, e.name, got))
			}
		}
		if !agree {
			continue
		}
		switch inj.Side {
		case "majority":
			// Overridden only if a salvaged victim tx on the same slot landed later.
			if !strings.EqualFold(first, inj.Value) && !v.slotOverwrittenBySalvage(ctx, injections, inj) {
				problems = append(problems, fmt.Sprintf("storage %s[%s] = %s, want majority value %s",
					inj.Contract, inj.Slot, first, inj.Value))
			}
		case "victim":
			// Inclusion fate: receipt present anywhere canonical = salvage.
			if rcpt, _ := v.getReceipt(ctx, v.els[0], inj.TxHash); rcpt != nil {
				salvaged++
			} else {
				evicted++
			}
		}
		// The block that first carried a victim-side injection must not be canonical anywhere.
		if inj.IslandBlock != "" {
			for _, e := range v.els {
				blk, err := v.getBlockByHash(ctx, e, inj.IslandBlock)
				if err != nil || blk == nil {
					continue // gone: the expected outcome
				}
				canon, err := v.getBlock(ctx, e, fmt.Sprintf("0x%x", uint64(blk.Number)))
				if err == nil && strings.EqualFold(canon.Hash.Hex(), blk.Hash.Hex()) {
					problems = append(problems, fmt.Sprintf("%s still holds victim-island block %s as canonical at height %d",
						e.name, inj.IslandBlock, uint64(blk.Number)))
				}
			}
		}
	}
	if len(problems) > 0 {
		return verdictFail, strings.Join(problems, "; ")
	}
	notes = append(notes, fmt.Sprintf("%d injection(s) verified byte-identical across %d node(s)", len(injections), len(v.els)))
	if salvaged+evicted > 0 {
		notes = append(notes, fmt.Sprintf("victim-side txs: %d salvaged, %d evicted (both legal)", salvaged, evicted))
	}
	return verdictPass, strings.Join(notes, "; ")
}

// slotOverwrittenBySalvage reports whether a victim-side injection on the same
// contract+slot was salvaged into the canonical chain.
func (v *verifier) slotOverwrittenBySalvage(ctx context.Context, all []migmon.Injection, maj migmon.Injection) bool {
	for _, inj := range all {
		if inj.Side != "victim" || inj.Contract != maj.Contract || inj.Slot != maj.Slot {
			continue
		}
		if rcpt, _ := v.getReceipt(ctx, v.els[0], inj.TxHash); rcpt != nil {
			return true
		}
	}
	return false
}

func (v *verifier) getStorageAt(ctx context.Context, e el, addr, slot string) (string, error) {
	var out string
	if err := v.fetch(ctx, e.url, "eth_getStorageAt", &out, addr, slot, "latest"); err != nil {
		return "", err
	}
	return out, nil
}

// rpcReceipt: nil receipt means the tx is not canonical.
type rpcReceipt struct {
	BlockHash string `json:"blockHash"`
}

func (v *verifier) getReceipt(ctx context.Context, e el, txHash string) (*rpcReceipt, error) {
	var out *rpcReceipt
	if err := v.fetch(ctx, e.url, "eth_getTransactionReceipt", &out, txHash); err != nil {
		return nil, err
	}
	return out, nil
}
