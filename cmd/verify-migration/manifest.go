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

// lapManifest is the lap driver's own record of what it DID: which profile
// it launched, whether it executed the host-side restart, which scenarios it
// asked the reorg service for and how each one ended, and when it quiesced
// the post-switchover chaos before judging. The verifier reconciles this
// against the evidence streams, so a lap step that silently never ran (a
// dead endpoint, a typo'd scenario name, a crashed restart) turns into a
// failed check instead of a thinner-looking green run.
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
	HandoverExpected bool  `json:"handover_expected"`
	QuiescedAt       int64 `json:"quiesced_at"` // unix seconds, 0 = never quiesced
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

// checkC15 reconciles the lap manifest against the run's evidence. Every
// expectation the manifest records is mandatory: INCONCLUSIVE is reserved
// for dice that never rolled, not for machinery that never showed up.
func (v *verifier) checkC15(ctx context.Context) (verdict, string) {
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
		// The restart must have landed in schedule-clear time: inside an
		// admitted op's window the node's outage would contaminate that
		// op's evidence, and after Quiet the gate owns the network.
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

// checkC18 judges the engineered state injected around the straddle. The
// chaos driver deploys a contract on the canonical chain before the
// fork-spanning partition, then writes CONFLICTING values to the same slots
// from both islands while they are split. After the heal there is exactly
// one right answer per slot on the canonical chain, and every node must
// give it: this turns the doomed branch's state from whatever traffic
// happened to land there into known values whose fate is checkable.
//
// Victim-side transactions are judged tolerantly on inclusion: a healed
// node's txpool may legally re-inject them into later canonical blocks
// (salvage) or drop them (fee eviction). What is never legal is nodes
// DISAGREEING about the resulting state, or the victim-island block that
// first held them staying canonical anywhere.
func (v *verifier) checkC18(ctx context.Context) (verdict, string) {
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
		// Cross-node agreement on the touched slot is the differential
		// assertion; the majority-side value winning is the chain-logic one.
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
			// The majority island's write must be the canonical outcome
			// unless a salvaged victim tx targeting the same slot landed
			// LATER and overwrote it - which the victim record for that
			// slot below will account for.
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
		// The island block that first included a victim-side tx must not
		// be canonical anywhere: it was minted on the doomed branch.
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

// slotOverwrittenBySalvage reports whether a victim-side injection targeting
// the same contract+slot was salvaged into the canonical chain, which
// legally supersedes the majority island's write if it executed later.
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

// rpcReceipt is the one field of a transaction receipt this package needs:
// whether the tx is included at all (nil receipt = not canonical).
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
