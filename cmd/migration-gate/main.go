// Command migration-gate holds a service back until every execution
// client has finished migrating, then runs one partition of its own and
// execs the command it wraps.
//
// It exists because the reorg service written for a chain that runs the
// binary tree from genesis knows nothing about an activation boundary: its
// cadence would happily partition the network while the migration is still
// converging. Gating it on completion keeps disruptoor under exactly one
// owner at a time, provable from the timeline alone.
//
// The partition it runs itself is not decoration. Before the fork, a reorg
// exercises merkle-canonical execution with a binary shadow being built
// behind it; after the migration completes, the same reorg exercises
// binary-canonical execution with the shadow retired. Those are different
// code paths, and only this side of the handoff can reach the second one
// while the stake layout still makes a deep reorg survivable.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

const (
	// donePoll is how often every client is asked whether it has finished.
	donePoll = 10 * time.Second
	// settle gives the network a moment after the last client reports done
	// before anything disturbs it again.
	settle = 60 * time.Second
	// watchPoll is how often the watchdog compares canonical chains.
	watchPoll = 30 * time.Second
	// deepWindow and shortWindow are the post-migration partition lengths,
	// matched to the pre-fork schedule so the two sides of the handoff are
	// comparable: a heavy victim reaches a deep branch in the first, a
	// light victim heals quickly in the second.
	deepWindow  = 190 * time.Second
	shortWindow = 150 * time.Second
)

type elFlag struct {
	names []string
	urls  []string
}

func (e *elFlag) String() string { return strings.Join(e.names, ",") }
func (e *elFlag) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("want name=url, got %q", v)
	}
	e.names = append(e.names, name)
	e.urls = append(e.urls, url)
	return nil
}

type intsFlag struct{ vals []int }

func (f *intsFlag) String() string { return fmt.Sprint(f.vals) }
func (f *intsFlag) Set(v string) error {
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fmt.Errorf("not a participant index: %q", v)
	}
	f.vals = append(f.vals, n)
	return nil
}

func main() {
	var (
		els       elFlag
		protected intsFlag
		api       = flag.String("disruptoor", "", "disruptoor native API base URL")
		genesisTS = flag.Int64("genesis-time", 0, "chain genesis unix time")
		forkTS    = flag.Int64("binary-trie-time", 0, "tree activation unix time")
		profile   = flag.String("profile", "", "the chaos profile this run uses: "+strings.Join(migsched.Names(), ", "))
		heavy     = flag.Int("heavy-node", 0, "participant index holding the heavy validator share")
		share     = flag.Float64("heavy-share", 0.40, "that participant's share of the validator set")
		slotSecs  = flag.Int("seconds-per-slot", 6, "chain slot duration")
		postOp    = flag.String("post-op", "deep-heavy", "partition to run once migration completes: none, short-light, deep-heavy")
		jsonlPath = flag.String("jsonl", "", "JSONL event path (default stdout)")
	)
	flag.Var(&els, "el", "execution client as name=url, repeatable; order is the participant index")
	flag.Var(&protected, "protect-node", "participant index never isolated, repeatable")
	flag.Parse()

	wrapped := flag.Args()
	if len(els.names) == 0 || *forkTS == 0 || *genesisTS == 0 || *profile == "" || *heavy == 0 {
		fmt.Fprintln(os.Stderr, "required: --el (>=1), --genesis-time, --binary-trie-time, --profile, --heavy-node")
		os.Exit(2)
	}
	if *postOp != "none" && *postOp != "short-light" && *postOp != "deep-heavy" {
		fmt.Fprintf(os.Stderr, "unknown --post-op %q\n", *postOp)
		os.Exit(2)
	}

	out := os.Stdout
	if *jsonlPath != "" && *jsonlPath != "/dev/stdout" {
		f, err := os.OpenFile(*jsonlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "jsonl: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		out = f
	}
	log := migmon.NewLog(out)

	genesis, fork := time.Unix(*genesisTS, 0), time.Unix(*forkTS, 0)
	lights := lightNodes(len(els.names), *heavy, protected.vals)
	sched, err := migsched.Resolve(*profile, genesis, fork, migsched.Topology{
		Heavy:          *heavy,
		Lights:         lights,
		HeavyShare:     *share,
		SecondsPerSlot: *slotSecs,
	})
	if err != nil {
		// Without the schedule the watchdog cannot tell a held partition
		// from a wedged node, and healing the wrong one either destroys
		// the evidence or leaves the network split.
		fmt.Fprintf(os.Stderr, "schedule: %v\n", err)
		os.Exit(2)
	}

	clients := make([]migmon.Client, 0, len(els.names))
	for i, name := range els.names {
		clients = append(clients, migmon.NewClient(name, els.urls[i]))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var d *disruptoor.Client
	if *api != "" {
		d = disruptoor.New(*api, 15*time.Second)
	}

	// The watchdog runs for the whole of this process's life, including
	// while it waits for the migration to finish: a partition left applied
	// by a crashed driver would otherwise sit there unnoticed.
	if d != nil {
		go watchdog(ctx, log, d, clients, sched)
	}

	if !waitForDone(ctx, log, clients) {
		return // signalled; nothing has been disturbed
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "every client reports the migration done"})

	if d != nil && *postOp != "none" {
		if !sleep(ctx, settle) {
			return
		}
		if err := runPostOp(ctx, log, d, clients, *postOp, *heavy, lights, len(els.names)); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvCritical, Finding: migmon.FindingNoConvergence, Detail: err.Error()})
			os.Exit(1)
		}
	}

	log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "handing disruptoor over to the wrapped service"})
	if len(wrapped) == 0 {
		return
	}
	// Exec rather than spawn: the wrapped service becomes this container's
	// process, so ownership of disruptoor passes with no overlap and no
	// second process to reap.
	bin, err := exec.LookPath(wrapped[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "wrapped command %q: %v\n", wrapped[0], err)
		os.Exit(127)
	}
	if err := syscall.Exec(bin, wrapped, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "exec %s: %v\n", bin, err)
		os.Exit(127)
	}
}

// lightNodes returns the disruptable participants that are neither the
// heavy victim nor protected.
func lightNodes(count, heavy int, protect []int) []int {
	blocked := map[int]bool{heavy: true}
	for _, n := range protect {
		blocked[n] = true
	}
	var out []int
	for i := 1; i <= count; i++ {
		if !blocked[i] {
			out = append(out, i)
		}
	}
	return out
}

// waitForDone blocks until every client that can report migration progress
// says it is done. A client with no introspection surface cannot answer, so
// it is not counted - and that omission is logged once, because a run whose
// completion is inferred from a subset of nodes proved less than it looks.
func waitForDone(ctx context.Context, log *migmon.Log, clients []migmon.Client) bool {
	var watched []migmon.Client
	for _, c := range clients {
		if c.Introspects() {
			watched = append(watched, c)
			continue
		}
		log.Emit(migmon.Event{
			Kind: migmon.EvWarn, Node: c.Name(),
			Detail: "no migration introspection: completion is judged without this client",
		})
	}
	if len(watched) == 0 {
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Finding: migmon.FindingBoundary,
			Detail: "no client can report migration progress; completion cannot be observed",
		})
		return false
	}

	stalled := map[string]bool{}
	for {
		done := 0
		for _, c := range watched {
			raw, err := c.Progress(ctx)
			if err == nil {
				var prog migmon.MigrationProgress
				if prog, err = migmon.DecodeProgress(raw); err == nil {
					delete(stalled, c.Name())
					if prog.Phase == migmon.PhaseDone {
						done++
					}
					continue
				}
			}
			// Completion cannot be confirmed without this client, so keep
			// waiting - but say so once, or the run looks merely slow.
			if !stalled[c.Name()] {
				stalled[c.Name()] = true
				log.Emit(migmon.Event{
					Kind: migmon.EvWarn, Node: c.Name(),
					Detail: fmt.Sprintf("migration progress unreadable, completion is blocked on it: %v", err),
				})
			}
		}
		if done == len(watched) {
			return true
		}
		if !sleep(ctx, donePoll) {
			return false
		}
	}
}

// runPostOp applies one partition after the migration has completed, heals
// it, and waits for the network to agree again.
func runPostOp(ctx context.Context, log *migmon.Log, d *disruptoor.Client, clients []migmon.Client, kind string, heavy int, lights []int, participants int) error {
	victim, window := heavy, deepWindow
	if kind == "short-light" {
		if len(lights) == 0 {
			return fmt.Errorf("post-migration short needs a light participant; none are disruptable")
		}
		victim, window = lights[0], shortWindow
	}
	if n, err := d.Containers(); err != nil || n == 0 {
		return fmt.Errorf("disruptoor sees %d containers (err=%v); a partition here would change no traffic", n, err)
	}

	name := fmt.Sprintf("post-migration-%s", kind)
	if err := d.Partition(name, others(participants, victim), []int{victim}); err != nil {
		return fmt.Errorf("partitioning node %d after the migration: %w", victim, err)
	}
	log.Emit(migmon.Event{
		Kind: migmon.EvIsolate, Node: nodeName(victim),
		Detail: fmt.Sprintf("class=%s window=%.0fs after the migration completed", kind, window.Seconds()),
	})

	if !sleep(ctx, window) {
		d.Clear()
		return nil
	}
	if err := d.Clear(); err != nil {
		return fmt.Errorf("healing the post-migration partition: %w", err)
	}
	log.Emit(migmon.Event{Kind: migmon.EvHeal, Node: nodeName(victim), Detail: "post-migration window closed"})

	return awaitConvergence(ctx, log, clients)
}

// awaitConvergence waits for every client to agree on the canonical chain
// again. A post-migration reorg that never converges is the failure this
// whole run is looking for, so it is an error rather than a warning.
func awaitConvergence(ctx context.Context, log *migmon.Log, clients []migmon.Client) error {
	deadline := time.Now().Add(migmon.ConvergenceGrace * time.Second)
	for {
		agreed, detail, err := converged(ctx, clients)
		if err == nil && agreed {
			log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "every client agrees on the canonical chain again"})
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clients still disagree %ds after the heal: %s", migmon.ConvergenceGrace, detail)
		}
		if !sleep(ctx, 5*time.Second) {
			return nil
		}
	}
}

// converged reports whether every client has the same canonical hash at the
// shallowest common height.
func converged(ctx context.Context, clients []migmon.Client) (bool, string, error) {
	var min uint64
	first := true
	for _, c := range clients {
		h, err := c.HeadNumber(ctx)
		if err != nil {
			return false, "", err
		}
		if first || h < min {
			min, first = h, false
		}
	}
	if first {
		return false, "", fmt.Errorf("no client answered")
	}
	seen := map[string]string{}
	for _, c := range clients {
		hdr, err := c.HeaderByNumber(ctx, min)
		if err != nil || hdr == nil {
			return false, "", fmt.Errorf("%s has no block %d", c.Name(), min)
		}
		seen[hdr.Hash] = c.Name()
	}
	if len(seen) == 1 {
		return true, "", nil
	}
	var parts []string
	for hash, node := range seen {
		parts = append(parts, node+"="+hash)
	}
	return false, fmt.Sprintf("block %d: %s", min, strings.Join(parts, " vs ")), nil
}

// watchdog heals a partition nobody is holding. Divergence inside a
// scheduled window is the schedule doing its job; divergence outside every
// window, persisting past the convergence allowance, means a driver died
// with a partition applied - and the run is lost unless someone clears it.
func watchdog(ctx context.Context, log *migmon.Log, d *disruptoor.Client, clients []migmon.Client, sched migsched.Schedule) {
	var since time.Time
	fired := false
	for {
		if !sleep(ctx, watchPoll) {
			return
		}
		now := time.Now()
		agreed, detail, err := converged(ctx, clients)
		if err != nil {
			continue // a node being briefly unreachable is not divergence
		}
		if agreed {
			since, fired = time.Time{}, false
			continue
		}
		if sched.Covers(now) {
			// A window the schedule is holding: expected, and healing it
			// here would destroy the evidence it exists to produce.
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = now
			continue
		}
		if fired || now.Sub(since) <= migmon.ConvergenceGrace*time.Second {
			continue
		}
		fired = true
		if err := d.Clear(); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "watchdog heal failed: " + err.Error()})
			continue
		}
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Finding: migmon.FindingNoConvergence,
			Detail: fmt.Sprintf("healed a partition no schedule was holding after %.0fs of divergence: %s",
				now.Sub(since).Seconds(), detail),
		})
	}
}

// sleep waits d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func others(participants, victim int) []int {
	var out []int
	for i := 1; i <= participants; i++ {
		if i != victim {
			out = append(out, i)
		}
	}
	return out
}

func nodeName(idx int) string { return fmt.Sprintf("node-%d", idx) }
