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
	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

func main() {
	var els cli.MultiFlag
	flag.Var(&els, "el", "execution client as name=url (repeatable; at least one)")
	binaryTrieTime := flag.Int64("binary-trie-time", 0, "unix time the binary trie activates (required)")
	poll := flag.Duration("poll", 2*time.Second, "how often to poll debug_migrationProgress and eth_blockNumber")
	sampleInterval := flag.Duration("sample-interval", 60*time.Second, "how often to draw a cross-node shadow-root sample")
	jsonlPath := flag.String("jsonl", "", "path to write JSONL events (default stdout)")
	httpAddr := flag.String("http", "", "serve a live status page on this address, e.g. :8080 (default off)")
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
	snap := newSnapshot(binaryTrieT)
	// snap.Write decodes the same JSONL lines as w, keeping the live view
	// in lockstep without a second emit call per finding.
	jsonl := migmon.NewLog(io.MultiWriter(w, snap))
	quorum := migmon.NewIStarQuorum(binaryTrieT)
	split := &splitWatch{}
	resample := newResampleQueue()
	if singleImplementation(states) {
		// Single implementation: cross-node checks prove determinism only.
		jsonl.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor",
			Detail: "single-implementation run: cross-node checks prove determinism, not spec agreement"})
	}
	if *httpAddr != "" {
		serveHTTP(*httpAddr, snap, jsonl)
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
			snap.setNode(ns)
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
			sampleOnce(ctx, jsonl, states, split, snap, resample)
		}
	}
}
