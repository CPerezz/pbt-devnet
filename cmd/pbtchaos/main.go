// Command pbtchaos makes reorgs happen on purpose.
//
// A periodic isolation fork cuts the p2p of whichever node proposes next, for the two slots
// around its duty: it builds a block nobody receives, takes it as head, and unwinds when the
// isolation lifts. Scenarios go further and put SPECIFIC state on the doomed branch, then
// check every client agrees it is gone -- which is what needs two implementations, since a
// client that keeps that state disagrees with one that does not.
//
// Every job goes through one queue, so two disruptions never overlap. `make split` and
// `make heal` write to disruptoor directly and are outside it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
)

type config struct {
	slotSeconds   time.Duration
	validatorsPer uint64
	// validatorCounts holds one validator count per participant, in 1-based order, for a
	// chain whose stake is not shared out evenly. Empty means it is, and validatorsPer
	// describes every participant.
	validatorCounts []uint64
	nodeCount       int
	isolateEnabled  bool
	isolateMin      uint64
	isolateMax      uint64
	isolateFor      time.Duration
	defaultDepth    uint64
	// maxDepth caps how many blocks a reorg is allowed to reach, both for the periodic
	// isolation cadence and for scenario depth (including the HTTP override). Zero means
	// unbounded, which is today's behaviour.
	maxDepth  uint64
	protected map[int]bool
}

// parseValidatorCounts turns a comma-separated list into one validator count per
// participant, 1-based order. Every entry must be a positive integer: a zero or negative
// count would make a participant own no validators, or a negative range, neither of which
// is a stake a real node can hold.
func parseValidatorCounts(s string) ([]uint64, error) {
	fields := strings.Split(s, ",")
	counts := make([]uint64, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("wants a comma-separated list of positive integers, got %q", s)
		}
		counts = append(counts, n)
	}
	return counts, nil
}

// clampDepth applies --max-depth to a requested reorg depth. maxDepth of zero leaves the
// request untouched, which keeps every existing caller byte-identical when the flag is
// absent.
func clampDepth(requested, maxDepth uint64) (applied uint64, clamped bool) {
	if maxDepth == 0 || requested <= maxDepth {
		return requested, false
	}
	return maxDepth, true
}

// resolveDepth parses the depth query parameter, defaulting to def when it is absent, and
// applies --max-depth. requested is the depth actually asked for, before any clamp, so a
// caller can tell an operator what happened rather than just what was applied.
func resolveDepth(v string, def, maxDepth uint64) (applied, requested uint64, clamped bool, err error) {
	requested = def
	if v != "" {
		parsed, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil {
			return 0, 0, false, fmt.Errorf("bad depth: %s", v)
		}
		requested = parsed
	}
	applied, clamped = clampDepth(requested, maxDepth)
	return applied, requested, clamped, nil
}

// chaos is the single owner of disruptoor state. Every disruption runs on the worker
// goroutine, serialised through jobs.
type chaos struct {
	d    *disruptoor.Client
	els  []*el
	cls  []*beacon
	keys []string
	cfg  config
	log  *slog.Logger

	jobs chan job

	// quiesced stops NEW disruptions while letting a running job finish. The verifier
	// judges end-state agreement after the run; judging while the cadence keeps cutting
	// the network measures this driver, not the clients.
	quiesced atomic.Bool

	mu      sync.Mutex
	running string
	queued  []string
	history []result

	// Coverage, so "we are not always reorging the same client" is a number rather than
	// an impression.
	turn         int            // rotates the minority and the sender keys
	reorgsBy     map[string]int // client -> reorgs observed on it
	minorityRuns map[string]int // client -> scenarios run with it as the doomed branch
}

// eligible returns the 1-based node indices pbtchaos is allowed to disrupt.
//
// ethereum-package makes participant 1 the sole consensus bootnode and gives it no boot nodes
// of its own, so a partition strands it with nothing to rediscover through: it sits at zero
// peers for the rest of the run and every later scenario measures that instead of a reorg.
func (c *chaos) eligible() []int {
	out := make([]int, 0, len(c.els))
	for i := range c.els {
		if !c.cfg.protected[i+1] {
			out = append(out, i+1)
		}
	}
	return out
}

// nextMinority advances the rotation and returns a 1-based node index.
func (c *chaos) nextMinority() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.eligible()
	if len(e) == 0 {
		return 1
	}
	idx := e[c.turn%len(e)]
	c.turn++
	return idx
}

// minorityFor resolves an operator-pinned minority pick, falling back to the rotation when
// the pick is out of range or protected. A pinned protected node must never proceed as the
// doomed branch -- the rotation is what already knows how to choose an eligible one instead.
func (c *chaos) minorityFor(pick int) int {
	if pick < 1 || pick > len(c.els) || c.cfg.protected[pick] {
		return c.nextMinority()
	}
	return pick
}

// keysFor hands each run a majority/minority pair, walking the pool so runs do not repeat
// the same pair.
//
// With the pool at three -- the most that fits below the hammer's slice without reaching the
// accounts spamoor and assertoor claim -- consecutive runs necessarily share ONE key: two
// drawn from three cannot be disjoint. Widening the pool is what would make them disjoint,
// and the guard in main.star is what stops it.
func (c *chaos) keysFor() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) == 1 {
		return c.keys[0], c.keys[0]
	}
	a := c.turn * 2 % len(c.keys)
	b := (c.turn*2 + 1) % len(c.keys)
	return c.keys[a], c.keys[b]
}

func (c *chaos) countMinority(client string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minorityRuns[client]++
}

func (c *chaos) countReorg(client string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reorgsBy[client]++
}

type job struct {
	name string
	run  func(context.Context) result
}

type result struct {
	Name    string `json:"name"`
	Started string `json:"started"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
	Reorged bool   `json:"reorged"`
	Depth   uint64 `json:"depth,omitempty"`
	// RequestedDepth is set only when --max-depth clamped the depth actually asked for,
	// so a clamped run is visible in the result rather than looking like an ordinary one.
	RequestedDepth uint64 `json:"requested_depth,omitempty"`
	Minority       string `json:"minority,omitempty"`
	Orphaned       int    `json:"orphaned_blocks,omitempty"`
	Survivor       string `json:"survivor,omitempty"`
}

func main() {
	var els, cls, keys, protect cli.MultiFlag
	api := flag.String("disruptoor", "", "disruptoor base URL (required)")
	flag.Var(&els, "el", "execution client as name=rpcURL (repeatable)")
	flag.Var(&cls, "cl", "consensus client as name=beaconURL (repeatable)")
	flag.Var(&keys, "key", "prefunded sender private key (repeatable)")
	flag.Var(&protect, "protect-node", "1-based node index never to disrupt (repeatable)")
	listen := flag.String("listen", ":7800", "control API listen address")
	slotSeconds := flag.Duration("slot-seconds", 12*time.Second, "seconds per slot")
	validatorsPer := flag.Uint64("validators-per-node", 128, "validators assigned to each participant")
	validatorCounts := flag.String("validator-counts", "", "comma-separated validator count per participant, 1-based order, for uneven stake (default: derive from --validators-per-node)")
	isolation := flag.Bool("isolation", true, "run the periodic proposer-isolation forks")
	isoMin := flag.Uint64("isolate-min-blocks", 15, "minimum blocks between isolation forks")
	isoMax := flag.Uint64("isolate-max-blocks", 30, "maximum blocks between isolation forks")
	isolateFor := flag.Duration("isolate-for", 0, "how long to isolate the proposer (default: two slots)")
	depth := flag.Uint64("depth", 10, "default scenario depth in blocks")
	maxDepth := flag.Uint64("max-depth", 0, "clamp every reorg, periodic and scenario, to this many blocks (0: unbounded)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *api == "" {
		cli.Fatal(log, "missing --disruptoor")
	}
	if len(els) == 0 {
		cli.Fatal(log, "missing --el")
	}
	if *isoMin > *isoMax {
		cli.Fatal(log, "--isolate-min-blocks (%d) is above --isolate-max-blocks (%d)", *isoMin, *isoMax)
	}

	// Two slots by default. The isolation starts once the chain reaches the slot BEFORE
	// the duty, so it has to span the rest of that slot plus the whole proposal slot;
	// one slot would lift it while the proposer was still publishing.
	if *isolateFor == 0 {
		*isolateFor = 2 * *slotSeconds
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	elc, err := dialELs(ctx, els)
	if err != nil {
		cli.Fatal(log, "%v", err)
	}
	clc, err := newBeacons(cls)
	if err != nil {
		cli.Fatal(log, "%v", err)
	}

	d := disruptoor.New(*api, 15*time.Second)
	// A selector matching nothing is accepted, changes no traffic, and leaves a healthy
	// chain behind -- the one failure that is indistinguishable from success. Refuse to
	// start rather than report imaginary reorgs for the rest of the run.
	n, err := d.Containers()
	if err != nil {
		cli.Fatal(log, "disruptoor at %s is unreachable: %v", *api, err)
	}
	if n == 0 {
		cli.Fatal(log, "disruptoor at %s sees no containers: every selector would match nothing", *api)
	}
	log.Info("disruptoor ready", "url", *api, "containers", n)

	// Start from a clean slate: a partition left behind by a previous run would make
	// the first scenario's verification meaningless.
	if err := d.Clear(); err != nil {
		cli.Fatal(log, "could not clear disruptoor state: %v", err)
	}

	protectedNodes := map[int]bool{}
	for _, p := range protect {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			cli.Fatal(log, "--protect-node wants a 1-based node index, got %q", p)
		}
		protectedNodes[n] = true
	}
	if len(protectedNodes) >= len(elc) {
		cli.Fatal(log, "every node is protected, so there is nothing to disrupt")
	}
	var counts []uint64
	if *validatorCounts != "" {
		parsed, err := parseValidatorCounts(*validatorCounts)
		if err != nil {
			cli.Fatal(log, "--validator-counts: %v", err)
		}
		if len(parsed) != len(elc) {
			cli.Fatal(log, "--validator-counts has %d entries but there are %d participants (--el given %d times)",
				len(parsed), len(elc), len(elc))
		}
		counts = parsed
	}

	c := &chaos{
		d: d, els: elc, cls: clc, keys: keys, log: log,
		jobs:         make(chan job, 16),
		reorgsBy:     map[string]int{},
		minorityRuns: map[string]int{},
		cfg: config{
			slotSeconds:     *slotSeconds,
			validatorsPer:   *validatorsPer,
			nodeCount:       len(elc),
			isolateEnabled:  *isolation,
			isolateMin:      *isoMin,
			isolateMax:      *isoMax,
			isolateFor:      *isolateFor,
			defaultDepth:    *depth,
			maxDepth:        *maxDepth,
			validatorCounts: counts,
			protected:       protectedNodes,
		},
	}

	go c.serve(ctx, *listen)
	if c.cfg.isolateEnabled {
		if len(clc) == 0 {
			log.Warn("isolation forks need --cl to read proposer duties; disabling them")
			c.cfg.isolateEnabled = false
		} else {
			go c.isolationLoop(ctx)
		}
	}

	c.work(ctx)

	// Never leave the network disrupted on the way out.
	if err := c.d.Clear(); err != nil {
		log.Error("could not clear disruptoor state on shutdown", "err", err)
	}
	log.Info("stopped")
}

// work runs jobs one at a time. This is what serialises the periodic forks against
// operator-triggered scenarios.
func (c *chaos) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.jobs:
			c.mu.Lock()
			c.running = j.name
			if len(c.queued) > 0 && c.queued[0] == j.name {
				c.queued = c.queued[1:]
			}
			c.mu.Unlock()

			res := j.run(ctx)

			c.mu.Lock()
			c.running = ""
			c.history = append(c.history, res)
			if len(c.history) > 50 {
				c.history = c.history[len(c.history)-50:]
			}
			c.mu.Unlock()

			// Whatever happened, the network goes back to normal before the next job.
			if err := c.d.Clear(); err != nil {
				c.log.Error("could not clear disruptoor state", "err", err)
			}
			repeerELs(context.Background(), c.els, c.log)
		}
	}
}

// errQuiesced marks a submission refused because the driver has been quiesced, so the
// HTTP handler can answer 409 rather than the 503 a full queue gets.
var errQuiesced = errors.New("quiesced: no new disruptions are accepted")

// submit queues a job, refusing rather than blocking when the queue is full: a backlog
// of disruptions is never what anyone wanted.
func (c *chaos) submit(j job) error {
	if c.quiesced.Load() {
		return errQuiesced
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.jobs <- j:
		c.queued = append(c.queued, j.name)
		return nil
	default:
		return fmt.Errorf("queue is full")
	}
}

func (c *chaos) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running != "" || len(c.queued) > 0
}

// mux builds the control API's routes. Split out from serve so tests can exercise
// handlers directly, with no real listener involved.
func (c *chaos) mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		body := map[string]any{
			"running":       c.running,
			"queued":        c.queued,
			"history":       c.history,
			"reorgs_by":     c.reorgsBy,
			"minority_runs": c.minorityRuns,
		}
		c.mu.Unlock()
		parts, shaping, err := c.d.State()
		if err == nil {
			body["applied_partitions"] = parts
			body["applied_shaping"] = shaping
		}
		writeJSON(w, http.StatusOK, body)
	})

	// quiesceBody reports whether the driver is quiesced and whether a job is
	// currently running, so a caller polling GET can tell "accepted, draining" from
	// "accepted, idle" without a second endpoint.
	quiesceBody := func() map[string]any {
		return map[string]any{"quiesced": c.quiesced.Load(), "idle": !c.busy()}
	}

	// The verifier judges end-state agreement across clients after the run. Judging
	// while the periodic isolation cadence keeps cutting the network mid-verification
	// would measure this driver's timing, not the clients' actual convergence, so
	// quiescing stops new disruptions from being scheduled without touching one already
	// running.
	mux.HandleFunc("POST /quiesce", func(w http.ResponseWriter, r *http.Request) {
		c.quiesced.Store(true)
		writeJSON(w, http.StatusOK, quiesceBody())
	})

	mux.HandleFunc("GET /quiesce", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, quiesceBody())
	})

	mux.HandleFunc("POST /scenario/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		sc, ok := scenarios[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown scenario " + name, "known": scenarioNames()})
			return
		}
		depth, requestedDepth, depthClamped, err := resolveDepth(r.URL.Query().Get("depth"), c.cfg.defaultDepth, c.cfg.maxDepth)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if depthClamped {
			c.log.Warn("clamping requested scenario depth", "scenario", name,
				"requested", requestedDepth, "max_depth", c.cfg.maxDepth, "applied", depth)
		}
		// Optional: pin the doomed node instead of taking the next in the rotation.
		minority := 0
		if v := r.URL.Query().Get("minority"); v != "" {
			parsed, err := strconv.Atoi(v)
			if err != nil || parsed < 1 || parsed > len(c.els) {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": fmt.Sprintf("minority must be 1..%d, got %q", len(c.els), v)})
				return
			}
			minority = parsed
		}
		err = c.submit(job{
			name: name,
			run: func(ctx context.Context) result {
				res := c.runScenario(ctx, sc, depth, minority)
				if depthClamped {
					res.RequestedDepth = requestedDepth
				}
				return res
			},
		})
		if errors.Is(err, errQuiesced) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		body := map[string]any{"queued": name, "depth": depth}
		if depthClamped {
			body["requested_depth"] = requestedDepth
		}
		writeJSON(w, http.StatusAccepted, body)
	})

	return mux
}

func (c *chaos) serve(ctx context.Context, addr string) {
	srv := &http.Server{Addr: addr, Handler: c.mux()}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sd)
	}()
	c.log.Info("control API listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		c.log.Error("control API stopped", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}
