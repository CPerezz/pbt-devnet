// Command migration-gate holds a service back until every execution
// client finishes migrating, then runs one partition of its own and
// execs the wrapped command. Gating keeps disruptoor under exactly one
// owner at a time; the reorg service otherwise has no activation boundary.
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
	donePoll  = 10 * time.Second // how often clients are polled for done
	settle    = 60 * time.Second // pause after done before disturbing the network
	watchPoll = 30 * time.Second // watchdog canonical-chain comparison interval
	// deepWindow/shortWindow match the pre-fork schedule so both sides of
	// the handoff are comparable.
	deepWindow  = 190 * time.Second
	shortWindow = 150 * time.Second
)

// held marks a partition this process holds itself, so the watchdog does
// not treat it as stuck.
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
		cls       elFlag
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
	flag.Var(&cls, "cl", "consensus client beacon API as name=url, repeatable")
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
		// A held partition and a wedged node need different treatment.
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

	// Watchdog runs for this process's whole life, including before done.
	mine := &held{}
	if d != nil {
		go watchdog(ctx, log, d, clients, sched, mine)
	}
	go peerWatch(ctx, log, cls, sched, mine)

	if !waitForDone(ctx, log, clients) {
		if ctx.Err() == nil {
			// A devnet where nothing can report done must not exit clean.
			os.Exit(1)
		}
		return // signalled; nothing has been disturbed
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "every client reports the migration done"})

	if d != nil && *postOp != "none" {
		if !sleep(ctx, settle) {
			return
		}
		// migration-chaos runs until sched.Quiet; wait for that exactly
		// rather than inferring quiet from an absence of chaos events.
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
	// Exec, not spawn: disruptoor ownership passes with no second process.
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

// waitForDone blocks until every introspectable client reports done. A
// client with no introspection surface is excluded and logged once.
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
			// Can't confirm completion without this client; keep waiting.
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

// runPostOp applies one partition after migration, heals it, and waits
// for the network to reconverge.
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
	// and would otherwise heal this as an unclaimed divergence.
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

	// Rebuild the peer mesh: this devnet's execution layer runs with
	// almost no peers of its own.
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
// again. On deadline expiry, if the head spread is still shrinking the
// wait is extended once by the same deadline; a static or growing spread
// fails immediately.
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

// spreadShrinking samples the head spread three times, 5s apart, and
// reports whether it is strictly narrowing.
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

// trendShrinking reports whether three spread samples strictly narrow.
func trendShrinking(samples []uint64) bool {
	return samples[2] < samples[1] && samples[1] < samples[0]
}

// headSpread returns the gap between the highest and lowest reported head.
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

// watchdog heals a partition nobody is holding. Inside a scheduled window
// divergence is expected; outside one, past the grace period, it means a
// driver died holding the partition open.
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
			// A window the schedule holds: expected, don't heal it.
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
		// State() before Clear() says whether a partition was actually
		// applied: distinguishes a dead driver from a mesh-level re-peer fix.
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

// healKind: State() read right before Clear() says whether a partition was
// applied. If so a driver died holding it; otherwise re-peering fixed it.
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

// peerStarvedAfter is how long a consensus client may sit at zero peers
// outside any scheduled partition before it is reported starved.
const peerStarvedAfter = 90 * time.Second

// starvation is one consensus client's zero-peer episode.
type starvation struct {
	since time.Time // first zero-peer observation of the episode
	fired bool
}

// observe updates the episode with one poll and reports whether to emit.
// Peers, an unreachable API, or a legally held partition all end the
// episode: a victim at zero peers inside its window is not starved.
func (s *starvation) observe(now time.Time, peers int, err error, held bool) bool {
	if err != nil || peers > 0 || held {
		*s = starvation{}
		return false
	}
	if s.since.IsZero() {
		s.since = now
		return false
	}
	if s.fired || now.Sub(s.since) < peerStarvedAfter {
		return false
	}
	s.fired = true
	return true
}

// peerWatch reports a consensus client left with no peers while the network
// is supposed to be whole. Partitions cut peers by design; what must not
// happen is a client failing to get them back after the heal - measured on
// the bootnode, which has no boot-nodes of its own to redial. Emitted as a
// warning once per episode; the lap driver restarts the client on it.
func peerWatch(ctx context.Context, log *migmon.Log, cls elFlag, sched migsched.Schedule, mine *held) {
	episodes := make([]starvation, len(cls.names))
	for {
		if !sleep(ctx, watchPoll) {
			return
		}
		now := time.Now()
		for i, url := range cls.urls {
			peers, err := migmon.BeaconPeerCount(ctx, url)
			if episodes[i].observe(now, peers, err, sched.Covers(now) || mine.holding(now)) {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Finding: migmon.FindingPeerStarved, Node: cls.names[i],
					Detail: fmt.Sprintf("no consensus peers for %.0fs outside any scheduled partition", now.Sub(episodes[i].since).Seconds())})
			}
		}
	}
}
