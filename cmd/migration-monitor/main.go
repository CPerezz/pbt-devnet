// Command migration-monitor watches N execution clients through the EIP-8347
// binary-trie migration and reports findings as it observes them.
//
// It polls debug_migrationProgress and eth_blockNumber every --poll tick,
// tracks each node's phase timeline against the binary-trie activation time
// (--binary-trie-time) for stall (F2) and boundary (F3) findings, and every
// --sample-interval draws a random-depth cross-node shadow-root sample for
// root-mismatch (F1), persistent-null, and reorg findings.
//
// It never moves forkchoice, never persists a block, and a CRITICAL finding
// never stops the process: this daemon observes, verify-migration judges.
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
	quorum := migmon.NewBStarQuorum(binaryTrieT)
	split := &splitWatch{}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pollTick := time.NewTicker(*poll)
	defer pollTick.Stop()
	sampleTick := time.NewTicker(*sampleInterval)
	defer sampleTick.Stop()

	doPoll := func() {
		for _, ns := range states {
			pollOnce(ctx, jsonl, binaryTrieT, ns, quorum)
		}
	}
	doPoll() // first poll immediately rather than waiting a full --poll interval

	for {
		select {
		case <-ctx.Done():
			// Nothing to flush explicitly: jsonl writes unbuffered, and the
			// deferred file Close (if --jsonl is a file) runs on return.
			return
		case <-pollTick.C:
			doPoll()
		case <-sampleTick.C:
			sampleOnce(ctx, jsonl, states, split)
		}
	}
}
