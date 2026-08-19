package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// receiptAudit samples what actually happened to the transactions this process sent.
//
// Without it the hammer is blind in the way the reorg scenarios used to be. It checked
// receipts for its three startup deployments and nothing else, so a workload whose
// transactions all reverted -- gas too low for a two-dimensional-gas operation is the easy
// way to get there -- kept sending forever and reported nothing. The devnet looks busy, the
// send counters climb, and a whole transaction shape is silently untested.
//
// Only a sample is followed: at a few transactions a second, checking every receipt would
// cost more RPC than the sending does, and a systematic failure shows up in a sample just as
// clearly as in the whole population.
type receiptAudit struct {
	mu      sync.Mutex
	pending map[string][]common.Hash // workload -> hashes awaiting a receipt
	mined   map[string]int
	revert  map[string]int
	// reported remembers what has already been said, so a workload that is broken for an
	// hour produces one finding rather than one every twenty-five rounds.
	reported map[string]bool
	every    int
	seen     map[string]int
}

func newReceiptAudit(sampleEvery int) *receiptAudit {
	if sampleEvery < 1 {
		sampleEvery = 1
	}
	return &receiptAudit{
		pending:  map[string][]common.Hash{},
		mined:    map[string]int{},
		revert:   map[string]int{},
		reported: map[string]bool{},
		seen:     map[string]int{},
		every:    sampleEvery,
	}
}

// watch records every Nth transaction of a workload for later inspection.
func (a *receiptAudit) watch(kind string, h common.Hash) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen[kind]++
	if a.seen[kind]%a.every != 0 {
		return
	}
	// Keep the queue short: if receipts are not being collected the chain has bigger
	// problems, and an unbounded list would just leak.
	if len(a.pending[kind]) < 8 {
		a.pending[kind] = append(a.pending[kind], h)
	}
}

// revertIsExpected is true for the one workload whose whole point is to revert after
// writing, so that its reverts are not a finding -- and its SUCCESS would be.
func revertIsExpected(kind string) bool { return kind == "revert" }

// report collects the sampled receipts and says what it found. A workload that only ever
// reverts when it should not, or never mines at all, is a finding: it means that shape is
// not being tested at all, which is worse than it failing loudly.
func (a *receiptAudit) report(ctx context.Context, cl *ethclient.Client) {
	a.mu.Lock()
	batch := map[string][]common.Hash{}
	for kind, hs := range a.pending {
		batch[kind] = hs
		a.pending[kind] = nil
	}
	a.mu.Unlock()

	for kind, hs := range batch {
		for _, h := range hs {
			rcpt, err := cl.TransactionReceipt(ctx, h)
			if err != nil {
				// Not mined yet, or mined on a branch that was reorged away. Neither is
				// a fault on its own -- this devnet reorgs on purpose.
				continue
			}
			a.mu.Lock()
			if rcpt.Status == types.ReceiptStatusSuccessful {
				a.mined[kind]++
			} else {
				a.revert[kind]++
			}
			a.mu.Unlock()
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	kinds := make([]string, 0, len(a.seen))
	for k := range a.seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var parts []string
	for _, k := range kinds {
		m, r := a.mined[k], a.revert[k]
		if m+r == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d/%d", k, m, m+r))

		switch {
		case revertIsExpected(k) && m > 0 && !a.reported["ok:"+k]:
			a.reported["ok:"+k] = true
			slog.Error("FINDING: the revert workload produced a successful receipt — it is "+
				"supposed to revert after writing, so this shape is no longer testing "+
				"intra-transaction rollback", "workload", k, "succeeded", m)
		case !revertIsExpected(k) && m == 0 && r >= 3 && !a.reported["rv:"+k]:
			a.reported["rv:"+k] = true
			slog.Error("FINDING: every sampled transaction of this workload reverted, so the "+
				"shape it exists to exercise is not being tested at all", "workload", k,
				"reverted", r)
		}
	}
	if len(parts) > 0 {
		slog.Info("receipts sampled (successful/total)", "by_workload", joinParts(parts))
	}
}

func joinParts(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// gasDisagreement collapses the eth_estimateGas difference between clients into one report
// plus a periodic summary.
//
// It is a real client difference and worth knowing about -- geth and besu price the same
// deployment 2.9% apart -- but it fires on nearly every estimate, and 188 identical findings
// in one run is exactly the noise that buries the next real one. That is the same failure
// the state-root monitor had.
type gasDisagreement struct {
	mu       sync.Mutex
	seen     int
	worstPct float64
	worst    string
	reported bool
}

var gasGap = &gasDisagreement{}

func (d *gasDisagreement) note(first, other uint64, client int, call ethereum.CallMsg) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen++

	lo, hi := first, other
	if lo > hi {
		lo, hi = hi, lo
	}
	pct := 0.0
	if lo > 0 {
		pct = float64(hi-lo) * 100 / float64(lo)
	}
	if pct > d.worstPct {
		d.worstPct = pct
		d.worst = fmt.Sprintf("client_0=%d client_%d=%d (%.2f%%, %d bytes of calldata)",
			first, client, other, pct, len(call.Data))
	}

	// The first one carries the detail worth having; after that only the tally matters.
	if !d.reported {
		d.reported = true
		slog.Error("FINDING: clients disagree on gas for the same call — reported once here, "+
			"then as a periodic tally so it cannot bury a new finding",
			"client_0", first, "client_"+fmt.Sprint(client), other,
			"gap_pct", fmt.Sprintf("%.2f", pct), "data_len", len(call.Data))
	}
}

func (d *gasDisagreement) report() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == 0 {
		return
	}
	slog.Warn("eth_estimateGas disagreements so far", "count", d.seen, "worst", d.worst)
}
