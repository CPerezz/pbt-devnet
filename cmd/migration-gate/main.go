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
	"sync"
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

// held marks a partition this process is holding itself, so the watchdog
// does not treat its own window as a stuck one and heal it mid-flight. The
// scheduled windows come from the shared schedule; this covers the one the
// gate applies after the migration, which is not in it.
type held struct {
	mu    sync.Mutex
	until time.Time
}

func (h *held) hold(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.until = time.Now().Add(d + migmon.ConvergenceGrace*time.Second)
}

func (h *held) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.until = time.Time{}
}

func (h *held) holding(t time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.until.IsZero() && t.Before(h.until)
}

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
	mine := &held{}
	if d != nil {
		go watchdog(ctx, log, d, clients, sched, mine)
	}

	if !waitForDone(ctx, log, clients) {
		if ctx.Err() == nil {
			// waitForDone only returns false with no live context error when
			// it never entered its poll loop at all: no client had a
			// migration surface to watch. A devnet where nothing can report
			// done is a misconfiguration, not a quiet success, and must not
			// exit clean.
			os.Exit(1)
		}
		return // signalled; nothing has been disturbed
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "every client reports the migration done"})

	if d != nil && *postOp != "none" {
		if !sleep(ctx, settle) {
			return
		}
		// migration-chaos may still be issuing its own scheduled disruptions until the
		// schedule's Quiet instant. The gate already resolved this schedule to run its
		// watchdog, so waiting on sched.Quiet is the cheap, exact option here -- no need
		// to infer quiet from an absence of chaos events when the deadline is already
		// known outright.
		if wait := time.Until(sched.Quiet); wait > 0 {
			log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: fmt.Sprintf("waiting %.0fs for the schedule to go quiet before the post-op partition", wait.Seconds())})
			if !sleep(ctx, wait) {
				return
			}
		}
		if err := runPostOp(ctx, log, d, clients, *postOp, *heavy, lights, len(els.names), mine); err != nil {
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
func runPostOp(ctx context.Context, log *migmon.Log, d *disruptoor.Client, clients []migmon.Client, kind string, heavy int, lights []int, participants int, mine *held) error {
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
	// Claim the window before applying it: the watchdog runs concurrently
	// and would otherwise see this divergence as a partition nobody is
	// holding and heal it a couple of minutes in.
	mine.hold(window)
	if err := d.Partition(name, others(participants, victim), []int{victim}); err != nil {
		mine.release()
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

	// Rebuild the peer mesh before waiting: a victim that missed blocks
	// needs peers to fetch them, and this devnet's execution layer runs
	// with almost none because the consensus clients carry the traffic.
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	if err := migmon.Repeer(rctx, clients); err != nil {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "re-peering after the heal: " + err.Error()})
	} else {
		log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "execution clients re-peered"})
	}

	err := awaitConvergence(ctx, log, clients)
	mine.release()
	return err
}

// awaitConvergence waits for every client to agree on the canonical chain
// again. A post-migration reorg that never converges is normally the
// failure this whole run is looking for, and an error. But a genuinely
// converging deep recovery must not be amputated just because it needed
// longer than the deadline sized for an ordinary heal: on deadline expiry
// this samples the head spread across ~3 more polls, and only if it is
// actually shrinking does it extend the wait once, by the same deadline.
// Static or growing spread means the split is not resolving on its own, and
// exec-ing the wrapped service onto it would produce evidence nobody could
// interpret, so that case still fails immediately.
func awaitConvergence(ctx context.Context, log *migmon.Log, clients []migmon.Client) error {
	deadline := time.Now().Add(migmon.ConvergenceGrace * time.Second)
	extended := false
	for {
		agreed, detail, err := converged(ctx, clients)
		if err == nil && agreed {
			log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "every client agrees on the canonical chain again"})
			return nil
		}
		if !time.Now().After(deadline) {
			if !sleep(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		if extended {
			return fmt.Errorf("clients still disagree %ds after the extended deadline: %s", 2*migmon.ConvergenceGrace, detail)
		}
		shrinking, trend, err := spreadShrinking(ctx, clients)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("clients still disagree %ds after the heal: %s", migmon.ConvergenceGrace, detail)
		}
		if !shrinking {
			return fmt.Errorf("clients still disagree %ds after the heal and the head spread is not shrinking (%s): %s",
				migmon.ConvergenceGrace, trend, detail)
		}
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Finding: migmon.FindingNoConvergence,
			Detail: fmt.Sprintf("clients still disagree %ds after the heal but the head spread is shrinking (%s); extending the wait once: %s",
				migmon.ConvergenceGrace, trend, detail),
		})
		deadline = time.Now().Add(migmon.ConvergenceGrace * time.Second)
		extended = true
	}
}

// spreadShrinking samples the spread between clients' head numbers three
// times, 5s apart, and reports whether it is strictly narrowing. A hard
// split advances both branches at close to the same rate, so its spread
// stays flat or grows; a laggard genuinely catching up after a heal narrows
// it every sample. Three samples is the least that shows a trend rather
// than noise from one poll racing a block.
func spreadShrinking(ctx context.Context, clients []migmon.Client) (bool, string, error) {
	samples := make([]uint64, 0, 3)
	for i := range 3 {
		if i > 0 && !sleep(ctx, 5*time.Second) {
			return false, "", fmt.Errorf("context ended while sampling the trend")
		}
		s, err := headSpread(ctx, clients)
		if err != nil {
			return false, "", err
		}
		samples = append(samples, s)
	}
	trend := fmt.Sprintf("%d -> %d -> %d", samples[0], samples[1], samples[2])
	return trendShrinking(samples), trend, nil
}

// trendShrinking reports whether three spread samples are strictly
// narrowing, sample over sample -- the shape a laggard catching up
// produces, distinct from the flat or growing shape of a hard split.
func trendShrinking(samples []uint64) bool {
	return samples[2] < samples[1] && samples[1] < samples[0]
}

// headSpread returns the difference between the highest and lowest head
// number reported by any client, as a cheap proxy for how far apart two
// branches have grown.
func headSpread(ctx context.Context, clients []migmon.Client) (uint64, error) {
	var min, max uint64
	first := true
	for _, c := range clients {
		h, err := c.HeadNumber(ctx)
		if err != nil {
			return 0, err
		}
		if first {
			min, max, first = h, h, false
			continue
		}
		if h < min {
			min = h
		}
		if h > max {
			max = h
		}
	}
	return max - min, nil
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
func watchdog(ctx context.Context, log *migmon.Log, d *disruptoor.Client, clients []migmon.Client, sched migsched.Schedule, mine *held) {
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
		if sched.Covers(now) || mine.holding(now) {
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
		// disruptoor only exposes Clear(), not which partition it held, but State()
		// still says whether one was actually applied right before the clear: if so,
		// some driver claimed a window and died holding it open (dead-driver); if none
		// was applied, the divergence was mesh-level and re-peering is what actually
		// fixed it, not the clear (mesh-repeer). That distinction is what the verifier
		// needs to tell "a driver got killed mid-partition" apart from "peers just
		// dropped off".
		partsBefore, _, stateErr := d.State()
		if err := d.Clear(); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "watchdog heal failed: " + err.Error()})
			continue
		}
		if err := migmon.Repeer(ctx, clients); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "re-peering after the watchdog heal: " + err.Error()})
		}
		prefix := healKind(partsBefore, stateErr)
		log.Emit(migmon.Event{
			Kind: migmon.EvCritical, Finding: migmon.FindingNoConvergence,
			Detail: prefix + fmt.Sprintf("healed a partition no schedule was holding after %.0fs of divergence: %s",
				now.Sub(since).Seconds(), detail),
		})
	}
}

// healKind names a watchdog heal by the evidence available: disruptoor
// exposes no memory of which partition it held, but State() taken right
// before Clear() still says whether one was applied. If so, some driver
// claimed a window and died holding it open; if none was applied (or the
// state read itself failed, leaving nothing to claim otherwise), the
// divergence was mesh-level and re-peering is what actually fixed it.
func healKind(partsBefore int, stateErr error) string {
	if stateErr == nil && partsBefore > 0 {
		return "dead-driver: "
	}
	return "mesh-repeer: "
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
