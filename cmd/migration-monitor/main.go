// Command migration-monitor watches N execution clients through the EIP-8347
// binary-trie migration and reports findings as it observes them. It never
// moves forkchoice or persists a block; a CRITICAL finding never stops the
// process, it only observes.
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

func main() {
	var els, cls cli.MultiFlag
	var protect cli.IntsFlag
	flag.Var(&els, "el", "execution client as name=url (repeatable; at least one)")
	flag.Var(&cls, "cl", "consensus client as name=beacon-url, for peer counts on the page (repeatable)")
	binaryTrieTime := flag.Int64("binary-trie-time", 0, "unix time the binary trie activates (required)")
	genesisTime := flag.Int64("genesis-time", 0, "unix time of the chain's genesis; the page's slot axis (required with --http)")
	slotSeconds := flag.Uint64("seconds-per-slot", 6, "slot duration, for the page's time axis")
	poll := flag.Duration("poll", 2*time.Second, "how often to poll debug_migrationProgress and eth_blockNumber")
	sampleInterval := flag.Duration("sample-interval", 60*time.Second, "how often to draw a cross-node shadow-root sample")
	jsonlPath := flag.String("jsonl", "", "path to write JSONL events (default stdout)")
	httpAddr := flag.String("http", "", "serve the live migration page on this address, e.g. :8080 (default off)")
	windowSlots := flag.Uint64("window", 900, "how many slots of blocks the page receives")
	disruptoorURL := flag.String("disruptoor", "", "disruptoor API base, for the page's partition bands (default off)")
	profile := flag.String("profile", "", "migsched profile the reorg service runs, for the page's schedule ribbon (default off)")
	anchor := flag.Int("anchor-node", 1, "participant holding the heavy validator share, never partitioned (with --profile)")
	anchorShare := flag.Float64("anchor-share", 0.40, "that participant's share of the validator set")
	flag.Var(&protect, "protect-node", "participant never partitioned (repeatable)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if len(els) == 0 {
		cli.Fatal(log, "need at least one --el entry")
	}
	if *binaryTrieTime <= 0 {
		cli.Fatal(log, "--binary-trie-time is required and must be a positive unix time")
	}
	binaryTrieT := uint64(*binaryTrieTime)

	var states []*nodeState
	for _, el := range els {
		name, url, ok := strings.Cut(el, "=")
		if !ok || name == "" || url == "" {
			cli.Fatal(log, "want --el name=url, got %q", el)
		}
		states = append(states, newNodeState(name, url, binaryTrieT))
	}

	var w io.Writer = os.Stdout
	if *jsonlPath != "" {
		f, err := os.Create(*jsonlPath)
		if err != nil {
			cli.Fatal(log, "open --jsonl: %v", err)
		}
		defer f.Close()
		w = f
	}
	jsonl := migmon.NewLog(w)
	var col *collector
	if *httpAddr != "" {
		if *genesisTime <= 0 || *slotSeconds == 0 {
			cli.Fatal(log, "--http needs --genesis-time and --seconds-per-slot for the slot axis")
		}
		sched, classOf := resolveSchedule(jsonl, *profile, *anchor, *anchorShare, protect, len(states), *genesisTime, *binaryTrieTime, *slotSeconds)
		var d *disruptoor.Client
		if *disruptoorURL != "" {
			d = disruptoor.New(*disruptoorURL, 5*time.Second)
		}
		col = newCollector(states, beacons(log, cls), d, sched, classOf, *anchor, uint64(*genesisTime), *slotSeconds, binaryTrieT, *windowSlots)
		// The collector decodes the same JSONL lines as w: the page's alerts stay in lockstep with the stream.
		jsonl = migmon.NewLog(io.MultiWriter(w, col))
		serveHTTP(*httpAddr, col.document, jsonl)
	}
	quorum := migmon.NewIStarQuorum(binaryTrieT)
	split := &splitWatch{}
	resample := newResampleQueue()
	if singleImplementation(states) {
		// Single implementation: cross-node checks prove determinism only.
		jsonl.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor",
			Detail: "single-implementation run: cross-node checks prove determinism, not spec agreement"})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pollTick := time.NewTicker(*poll)
	defer pollTick.Stop()
	sampleTick := time.NewTicker(*sampleInterval)
	defer sampleTick.Stop()

	doPoll := func() {
		for _, ns := range states {
			pollOnce(ctx, jsonl, binaryTrieT, ns, quorum, resample)
		}
		if col != nil {
			col.tick(ctx, jsonl)
		}
	}
	doPoll() // first poll immediately, not after a full --poll interval

	for {
		select {
		case <-ctx.Done():
			// jsonl writes unbuffered; nothing to flush.
			return
		case <-pollTick.C:
			doPoll()
		case <-sampleTick.C:
			sampleOnce(ctx, jsonl, states, split, resample)
		}
	}
}
