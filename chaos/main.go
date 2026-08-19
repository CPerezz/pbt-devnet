// Command pbtchaos makes reorgs happen on purpose.
//
// Two things drive it. A periodic isolation fork cuts the p2p of whichever node is about
// to propose, for the two slots around its duty: it still builds a block, but nobody
// receives it, so its own execution client takes that block as head while the rest build
// on the parent. When the isolation lifts the loser unwinds -- a reorg every 15-30 blocks
// with no operator involvement. On top of that,
// scenarios put SPECIFIC state on the branch that is about to die -- deployed code,
// 7702 delegations, fresh accounts, storage written and storage deleted -- and then
// check every client agrees about that state once the branch is gone.
//
// That second part is the point of having two implementations. go-ethereum#30 covers
// the code-zone cases as unit tests; here the same shapes run against geth and besu at
// once, where a client that keeps the doomed branch's state disagrees with one that
// does not.
//
// pbtchaos owns disruptoor state exclusively. Everything it does goes through one
// queue, so "no reorg while another is in flight" is enforced rather than hoped for --
// two overlapping disruptions produce a mess that proves nothing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type config struct {
	slotSeconds    time.Duration
	validatorsPer  uint64
	nodeCount      int
	isolateEnabled bool
	isolateMin     uint64
	isolateMax     uint64
	isolateFor     time.Duration
	defaultDepth   uint64
}

// chaos is the single owner of disruptoor state. Every disruption runs on the worker
// goroutine, serialised through jobs.
type chaos struct {
	d    *disruptoor
	els  []*el
	cls  []*beacon
	keys []string
	cfg  config
	log  *slog.Logger

	jobs chan job

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

// nextMinority advances the rotation and returns a 1-based node index.
func (c *chaos) nextMinority() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.turn%len(c.els) + 1
	c.turn++
	return idx
}

// keysFor hands each run its own majority/minority pair, walking the key pool so no two
// consecutive scenarios share a nonce sequence.
func (c *chaos) keysFor() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) == 1 {
		return c.keys[0], c.keys[0]
	}
	a := c.turn * 2 % len(c.keys)
	b := (c.turn*2 + 1) % len(c.keys)
	if a == b {
		b = (b + 1) % len(c.keys)
	}
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
	Name     string `json:"name"`
	Started  string `json:"started"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
	Reorged  bool   `json:"reorged"`
	Depth    uint64 `json:"depth,omitempty"`
	Minority string `json:"minority,omitempty"`
	Survivor string `json:"survivor,omitempty"`
}

func main() {
	var els, cls, keys multiFlag
	api := flag.String("disruptoor", "", "disruptoor base URL (required)")
	flag.Var(&els, "el", "execution client as name=rpcURL (repeatable)")
	flag.Var(&cls, "cl", "consensus client as name=beaconURL (repeatable)")
	flag.Var(&keys, "key", "prefunded sender private key (repeatable)")
	listen := flag.String("listen", ":7800", "control API listen address")
	slotSeconds := flag.Duration("slot-seconds", 12*time.Second, "seconds per slot")
	validatorsPer := flag.Uint64("validators-per-node", 128, "validators assigned to each participant")
	isolation := flag.Bool("isolation", true, "run the periodic proposer-isolation forks")
	isoMin := flag.Uint64("isolate-min-blocks", 15, "minimum blocks between isolation forks")
	isoMax := flag.Uint64("isolate-max-blocks", 30, "maximum blocks between isolation forks")
	isolateFor := flag.Duration("isolate-for", 0, "how long to isolate the proposer (default: two slots)")
	depth := flag.Uint64("depth", 10, "default scenario depth in blocks")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *api == "" {
		fatal(log, "missing --disruptoor")
	}
	if len(els) == 0 {
		fatal(log, "missing --el")
	}
	if *isoMin > *isoMax {
		fatal(log, "--isolate-min-blocks (%d) is above --isolate-max-blocks (%d)", *isoMin, *isoMax)
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
		fatal(log, "%v", err)
	}
	clc, err := newBeacons(cls)
	if err != nil {
		fatal(log, "%v", err)
	}

	d := newDisruptoor(*api)
	// A selector matching nothing is accepted, changes no traffic, and leaves a healthy
	// chain behind -- the one failure that is indistinguishable from success. Refuse to
	// start rather than report imaginary reorgs for the rest of the run.
	n, err := d.containers()
	if err != nil {
		fatal(log, "disruptoor at %s is unreachable: %v", *api, err)
	}
	if n == 0 {
		fatal(log, "disruptoor at %s sees no containers: every selector would match nothing", *api)
	}
	log.Info("disruptoor ready", "url", *api, "containers", n)

	// Start from a clean slate: a partition left behind by a previous run would make
	// the first scenario's verification meaningless.
	if err := d.clear(); err != nil {
		fatal(log, "could not clear disruptoor state: %v", err)
	}

	c := &chaos{
		d: d, els: elc, cls: clc, keys: keys, log: log,
		jobs:         make(chan job, 16),
		reorgsBy:     map[string]int{},
		minorityRuns: map[string]int{},
		cfg: config{
			slotSeconds:    *slotSeconds,
			validatorsPer:  *validatorsPer,
			nodeCount:      len(elc),
			isolateEnabled: *isolation,
			isolateMin:     *isoMin,
			isolateMax:     *isoMax,
			isolateFor:     *isolateFor,
			defaultDepth:   *depth,
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
	if err := c.d.clear(); err != nil {
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
			if err := c.d.clear(); err != nil {
				c.log.Error("could not clear disruptoor state", "err", err)
			}
		}
	}
}

// submit queues a job, refusing rather than blocking when the queue is full: a backlog
// of disruptions is never what anyone wanted.
func (c *chaos) submit(j job) error {
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

func (c *chaos) serve(ctx context.Context, addr string) {
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
		parts, shaping, err := c.d.state()
		if err == nil {
			body["applied_partitions"] = parts
			body["applied_shaping"] = shaping
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("POST /scenario/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		sc, ok := scenarios[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown scenario " + name, "known": scenarioNames()})
			return
		}
		depth := c.cfg.defaultDepth
		if v := r.URL.Query().Get("depth"); v != "" {
			parsed, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad depth: " + v})
				return
			}
			depth = parsed
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
		err := c.submit(job{
			name: name,
			run:  func(ctx context.Context) result { return c.runScenario(ctx, sc, depth, minority) },
		})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": name, "depth": depth})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
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

func fatal(log *slog.Logger, format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
