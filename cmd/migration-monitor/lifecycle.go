package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// Offline migration: cmd/migration-swap pushes each producer's and consumer's
// step through POST /api/lifecycle; the monitor seeds the rest from --offline
// and derives replaying/caught_up from the consumer's own cursor. The monitor
// has no docker visibility, so the swap's word is the only source of a step.

var (
	producerSteps = map[string]bool{migmon.StepExporting: true, migmon.StepExported: true, migmon.StepExportFailed: true}
	consumerSteps = map[string]bool{migmon.StepPending: true, migmon.StepDisconnecting: true, migmon.StepImporting: true,
		migmon.StepReconnecting: true, migmon.StepReplaying: true, migmon.StepCaughtUp: true,
		migmon.StepFailed: true, migmon.StepTimeout: true, migmon.StepSkipped: true}
	// The swap's own doing: a node in one of these steps is down or rewound on purpose.
	quietSteps = map[string]bool{migmon.StepDisconnecting: true, migmon.StepImporting: true,
		migmon.StepReconnecting: true, migmon.StepReplaying: true}
)

type lifecycle struct {
	producer     bool
	producerStep string
	importer     string
	reason       string
	since        int64
	anchor       uint64
	hash         string
	detail       string
}

// offline is the lifecycle of every participant; the HTTP handler writes it,
// the poll goroutine reads it, hence the lock. A nil *offline is the feature off.
type offline struct {
	mu     sync.Mutex
	names  []string // participant i+1's service name
	nodes  map[int]*lifecycle
	swaps  []int        // consumers, in swap order
	source map[int]int  // consumer -> producer
	reset  map[int]bool // reconnected consumers whose per-node state the next poll restarts
}

// parseOffline seeds the lifecycle from --offline; "" or enabled:false is off.
func parseOffline(raw string, names []string) (*offline, error) {
	if raw == "" {
		return nil, nil
	}
	var cfg migmon.Offline
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	o := &offline{names: names, nodes: map[int]*lifecycle{}, source: map[int]int{}, reset: map[int]bool{}}
	known := func(n int) error {
		if n < 1 || n > len(names) {
			return fmt.Errorf("node %d is not a participant (1..%d)", n, len(names))
		}
		return nil
	}
	for _, p := range cfg.Producers {
		if err := known(p.Node); err != nil {
			return nil, err
		}
		o.node(p.Node).producer = true
		for _, c := range p.Consumers {
			o.source[c] = p.Node
		}
	}
	for _, c := range cfg.Consumers {
		if err := known(c.Node); err != nil {
			return nil, err
		}
		// No since: pending is a seed, not an observation, so it never ages.
		o.node(c.Node).importer = migmon.StepPending
		o.swaps = append(o.swaps, c.Node)
	}
	for _, n := range cfg.OnlineOnly {
		if err := known(n.Node); err != nil {
			return nil, err
		}
		lc := o.node(n.Node)
		lc.importer, lc.reason = migmon.StepNoImporter, n.Reason
	}
	return o, nil
}

func (o *offline) node(n int) *lifecycle {
	lc := o.nodes[n]
	if lc == nil {
		lc = &lifecycle{}
		o.nodes[n] = lc
	}
	return lc
}

// anchorOf is a consumer's own anchor, else the one its producer exported.
func (o *offline) anchorOf(n int) (uint64, string) {
	if lc := o.nodes[n]; lc != nil && lc.anchor != 0 {
		return lc.anchor, lc.hash
	}
	if p := o.nodes[o.source[n]]; p != nil {
		return p.anchor, p.hash
	}
	return 0, ""
}

func (o *offline) emit(log *migmon.Log, n int, step, detail string) {
	anchor, hash := o.anchorOf(n)
	log.Emit(migmon.Event{Kind: migmon.EvLifecycle, Node: o.names[n-1], Phase: step, Number: anchor, Hash: hash, Detail: detail})
}

// apply records one pushed step.
func (o *offline) apply(log *migmon.Log, n int, step string, anchor uint64, hash, detail string) error {
	if n < 1 || n > len(o.names) {
		return fmt.Errorf("unknown node %d", n)
	}
	if !producerSteps[step] && !consumerSteps[step] {
		return fmt.Errorf("unknown step %q", step)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	lc := o.node(n)
	if anchor != 0 {
		lc.anchor, lc.hash = anchor, hash
	}
	if producerSteps[step] {
		lc.producer, lc.producerStep = true, step
	} else {
		if step == migmon.StepReconnecting && lc.importer != step {
			// The import rewound the cursor to the anchor: nothing seen before holds.
			o.reset[n] = true
		}
		lc.importer = step
	}
	lc.since, lc.detail = time.Now().Unix(), detail
	o.emit(log, n, step, detail)
	return nil
}

// takeReset reports, once, that node n just reconnected after an import.
func (o *offline) takeReset(n int) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.reset[n]
	delete(o.reset, n)
	return r
}

// quiet: node n is down or rewound by the swap itself, so it raises no findings.
func (o *offline) quiet(n int) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	lc := o.nodes[n]
	return lc != nil && (quietSteps[lc.importer] || lc.producerStep == migmon.StepExporting)
}

// derive moves a reconnected consumer to replaying while its binary cursor
// trails the chain tip (node 1's head) by more than migmon.CaughtUpLag blocks,
// to caught_up once within; the node's own head is as stale as the cursor right
// after the restart. It waits for the poll after the reset: a reading from before
// the stop would read as caught up.
func (o *offline) derive(log *migmon.Log, n int, ns *nodeState, tip uint64) {
	if o == nil || !ns.haveProgress || !ns.haveHead || ns.lastProgress.Binary == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	lc := o.nodes[n]
	if lc == nil || o.reset[n] || (lc.importer != migmon.StepReconnecting && lc.importer != migmon.StepReplaying) {
		return
	}
	cursor, tip := uint64(ns.lastProgress.Binary.Cursor), max(ns.lastHead, tip)
	step := migmon.StepCaughtUp
	if tip > cursor+migmon.CaughtUpLag {
		step = migmon.StepReplaying
	}
	if step == lc.importer {
		return
	}
	lc.importer, lc.since = step, time.Now().Unix()
	lc.detail = fmt.Sprintf("binary cursor %d, tip %d", cursor, tip)
	o.emit(log, n, step, lc.detail)
}

// fill copies node n's lifecycle into its view.
func (o *offline) fill(n int, v *nodeView) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	lc := o.nodes[n]
	if lc == nil {
		return
	}
	v.Producer, v.ProducerStep, v.Importer, v.ImporterReason = lc.producer, lc.producerStep, lc.importer, lc.reason
	v.StepSince, v.StepDetail = lc.since, lc.detail
	v.Anchor, _ = o.anchorOf(n)
}

// views lists the anchor marks and the swap queue, in swap order. Anchor
// marks are read straight off each producer's lc.anchor - the same value
// apply() just wrote, so a second copy would only drift.
func (o *offline) views() ([]anchorMark, []swapView) {
	if o == nil {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var anchors []anchorMark
	for n := 1; n <= len(o.names); n++ {
		if lc := o.nodes[n]; lc != nil && lc.producer && lc.anchor != 0 {
			anchors = append(anchors, anchorMark{Node: n, Number: lc.anchor, Hash: lc.hash})
		}
	}
	swaps := make([]swapView, 0, len(o.swaps))
	for _, n := range o.swaps {
		lc := o.nodes[n]
		anchor, _ := o.anchorOf(n)
		swaps = append(swaps, swapView{Node: n, Producer: o.source[n], Importer: lc.importer, Anchor: anchor, StepSince: lc.since})
	}
	return anchors, swaps
}

// lifecycleHandler serves POST /api/lifecycle: 204, or 400 naming the fault.
func lifecycleHandler(o *offline, log *migmon.Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req migmon.Lifecycle
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := o.apply(log, req.Node, req.Step, req.Anchor, req.Hash, req.Detail); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
