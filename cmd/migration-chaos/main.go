// Command migration-chaos drives a fixed partition schedule (internal/migsched) against a
// migrating devnet: pair and single-light windows before the fork, one straddle across it.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
	"github.com/ethereum/go-ethereum/crypto"
)

// elFlag collects repeated --el name=url flags; flag order is the 1-based participant index.
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

func main() {
	var (
		els       elFlag
		protected cli.IntsFlag
		api       = flag.String("disruptoor", "", "disruptoor native API base URL")
		genesisTS = flag.Int64("genesis-time", 0, "chain genesis unix time")
		forkTS    = flag.Int64("binary-trie-time", 0, "tree activation unix time")
		profile   = flag.String("profile", "", "schedule profile: "+strings.Join(migsched.Names(), ", "))
		anchor    = flag.Int("anchor-node", 1, "participant holding the heavy validator share; never a victim, every heal converges on its chain")
		share     = flag.Float64("anchor-share", 0.40, "that participant's share of the validator set")
		slotSecs  = flag.Int("seconds-per-slot", 6, "chain slot duration")
		dryRun    = flag.Bool("dry-run", false, "print the resolved schedule and exit")
		jsonlPath = flag.String("jsonl", "", "JSONL event path (default stdout)")
		keys      cli.MultiFlag
	)
	flag.Var(&keys, "key", "hex private key of a prefunded account for straddle state injection (repeatable; 2+ enables the injector)")
	flag.Var(&els, "el", "execution client as name=url, repeatable; order is the participant index")
	flag.Var(&protected, "protect-node", "participant index never isolated, repeatable")
	flag.Parse()

	if *genesisTS == 0 || *forkTS == 0 || *profile == "" || len(els.names) == 0 {
		fmt.Fprintln(os.Stderr, "required: --el (>=1), --genesis-time, --binary-trie-time, --profile")
		os.Exit(2)
	}
	if *anchor < 1 || *anchor > len(els.names) {
		fmt.Fprintf(os.Stderr, "anchor participant %d is outside the %d configured clients\n", *anchor, len(els.names))
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
	sched, err := migsched.Resolve(*profile, genesis, fork, migsched.Topology{
		Anchor:         *anchor,
		Lights:         migsched.Lights(len(els.names), *anchor, protected),
		Participants:   len(els.names),
		AnchorShare:    *share,
		SecondsPerSlot: *slotSecs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedule: %v\n", err)
		os.Exit(2)
	}

	// Published before use: readers need it for op class and heal deadline.
	publish(log, sched, genesis, fork, *anchor)

	if *dryRun {
		emitPlan(log, sched)
		return
	}
	if *api == "" {
		fmt.Fprintln(os.Stderr, "required outside --dry-run: --disruptoor")
		os.Exit(2)
	}

	d := disruptoor.New(*api, 15*time.Second)
	// A selector matching nothing looks like success; refuse to start.
	n, err := d.Containers()
	if err != nil || n == 0 {
		fmt.Fprintf(os.Stderr, "disruptoor sees %d containers (err=%v); refusing to run no-op chaos\n", n, err)
		os.Exit(1)
	}

	clients := make([]migmon.Client, 0, len(els.names))
	for i, name := range els.names {
		clients = append(clients, migmon.NewClient(name, els.urls[i]))
	}

	// The injector writes on the first island and on the anchor, whose chain wins the heal.
	var inj *injector
	if str, ok := sched.Straddle(); ok && len(keys) >= 2 {
		parsed := make([]*ecdsa.PrivateKey, 0, len(keys))
		for _, k := range keys {
			key, err := crypto.HexToECDSA(strings.TrimPrefix(k, "0x"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad --key: %v\n", err)
				os.Exit(2)
			}
			parsed = append(parsed, key)
		}
		if inj, err = newInjector(els.urls[str.Victims[0]-1], els.urls[*anchor-1], parsed); err != nil {
			fmt.Fprintf(os.Stderr, "injector: %v\n", err)
			os.Exit(2)
		}
	}
	watch := straddleWatch{fork: fork, anchor: clients[*anchor-1], clients: clients}
	run(log, d, sched, clients, inj, watch.probe())
}

// crossedFn reports whether a victim's head is past the fork on a block the anchor does not know.
type crossedFn func(ctx context.Context, victim int) (bool, error)

type straddleWatch struct {
	fork    time.Time
	anchor  migmon.Client
	clients []migmon.Client
}

func (w straddleWatch) probe() crossedFn {
	return func(ctx context.Context, victim int) (bool, error) {
		head, err := w.clients[victim-1].HeaderByTag(ctx, "latest")
		if err != nil {
			return false, err
		}
		if head == nil || int64(head.Time) < w.fork.Unix() {
			return false, nil
		}
		known, err := w.anchor.HeaderByHash(ctx, head.Hash)
		if err != nil {
			return false, err
		}
		return known == nil, nil
	}
}

// islands renders an op's disruptoor groups: the rest of the network, then one per victim when mutual.
func islands(participants int, o migsched.Op) [][]int {
	groups := [][]int{migsched.Others(participants, o.Victims)}
	if o.Mutual {
		for _, v := range o.Victims {
			groups = append(groups, []int{v})
		}
		return groups
	}
	return append(groups, o.Victims)
}

func publish(log *migmon.Log, s migsched.Schedule, genesis, fork time.Time, anchor int) {
	raw, err := json.Marshal(s.NewDump(genesis, fork, anchor))
	if err != nil {
		fmt.Fprintf(os.Stderr, "publishing the schedule: %v\n", err)
		os.Exit(1)
	}
	log.Emit(migmon.Event{Kind: migmon.EvSchedule, Detail: s.Profile, Raw: raw})
}

// emitPlan prints the schedule without touching disruptoor; Plan marks the records as never executed.
func emitPlan(log *migmon.Log, s migsched.Schedule) {
	for _, o := range s.Ops {
		kind, detail := migmon.EvIsolate, window(o)
		if !o.Admitted() {
			kind, detail = migmon.EvSkip, o.Refused
		}
		for _, v := range o.Victims {
			log.Emit(migmon.Event{Kind: kind, Node: migsched.NodeName(v), Detail: detail, Plan: true})
		}
	}
	for _, f := range s.Failsafes {
		log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "sweep at " + f.UTC().Format(time.RFC3339), Plan: true})
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "quiet from " + s.Quiet.UTC().Format(time.RFC3339), Plan: true})
}

// clock is the loop's notion of time, faked in tests; sleepUntil is false when shutdown interrupted it.
type clock struct {
	now        func() time.Time
	sleepUntil func(time.Time) bool
}

func run(log *migmon.Log, d *disruptoor.Client, s migsched.Schedule, clients []migmon.Client, inj *injector, probe crossedFn) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	clk := clock{now: time.Now, sleepUntil: func(t time.Time) bool { return sleepUntil(t, stop) }}
	if execute(log, d, s, clients, inj, probe, clk) {
		<-stop
	}
}

// execute runs the schedule as one serial action list; false means a shutdown signal cut it short.
func execute(log *migmon.Log, d *disruptoor.Client, s migsched.Schedule, clients []migmon.Client, inj *injector, probe crossedFn, clk clock) bool {
	for _, o := range s.Ops {
		if !o.Admitted() {
			for _, v := range o.Victims {
				log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: migsched.NodeName(v), Detail: o.Refused})
			}
		}
	}

	// A shutdown mid-window must not leave the partition applied.
	wait := func(t time.Time) bool {
		if clk.sleepUntil(t) {
			return true
		}
		d.Clear()
		return false
	}
	sleepFor := func(dur time.Duration) bool { return clk.sleepUntil(clk.now().Add(dur)) }

	healed := true               // the last op's heal is known to have worked
	applied := map[string]bool{} // ops disruptoor accepted; only these have anything to heal

	for _, a := range s.Actions() {
		switch a.Kind {
		case migsched.ActOpStart:
			// Deploy while the network is still whole, so it lands canonical.
			if inj != nil && a.Op.Class == migsched.ClassStraddle {
				if !wait(a.At.Add(-60 * time.Second)) {
					return false
				}
				ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
				if err := inj.deploy(ctx, log); err != nil {
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "injection deploy: " + err.Error()})
				}
				cancel()
			}
			if !wait(a.At) {
				return false
			}
			if a.StaleAt(clk.now()) {
				log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: migsched.NodeName(a.Op.Victims[0]),
					Detail: fmt.Sprintf("op %s window already closed at execution time; skipped, not run zero-length", a.Op.Name)})
				continue
			}
			if err := d.Partition(a.Op.Name, islands(len(clients), a.Op)...); err != nil {
				for _, v := range a.Op.Victims {
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: migsched.NodeName(v), Detail: "partition failed: " + err.Error()})
				}
				continue
			}
			applied[a.Op.Name] = true
			healed = false
			for _, v := range a.Op.Victims {
				log.Emit(migmon.Event{Kind: migmon.EvIsolate, Node: migsched.NodeName(v), Detail: window(a.Op)})
			}
			if inj != nil && a.Op.Class == migsched.ClassStraddle {
				// Let both sides mint a block first, then write conflicting state.
				if !wait(a.At.Add(15 * time.Second)) {
					return false
				}
				ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
				inj.splitWrites(ctx, log)
				cancel()
			}

		case migsched.ActOpEnd:
			if !wait(a.At) {
				return false
			}
			if !applied[a.Op.Name] {
				for _, v := range a.Op.Victims {
					log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: migsched.NodeName(v), Detail: "never applied: " + a.Op.Name})
				}
				continue
			}
			var crossed, unreachable map[int]bool
			if !a.Op.HoldUntil.IsZero() {
				var ok bool
				if crossed, unreachable, ok = holdUntilCrossed(log, a.Op, probe, s.SlotSeconds, clk.now, sleepFor); !ok {
					d.Clear()
					return false
				}
			}
			if err := d.Clear(); err != nil {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "heal failed: " + err.Error()})
				continue
			}
			healed = true
			for _, v := range a.Op.Victims {
				detail := "window closed"
				switch {
				case unreachable[v]:
					detail += "; unreachable during the hold"
				case !a.Op.HoldUntil.IsZero() && !crossed[v]:
					detail += "; never crossed the fork on its own branch"
				}
				log.Emit(migmon.Event{Kind: migmon.EvHeal, Node: migsched.NodeName(v), Detail: detail})
			}
			repeer(log, clients)

		case migsched.ActSweep:
			if !wait(a.At) {
				return false
			}
			err := d.Clear()
			switch {
			case err != nil:
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "failsafe heal failed: " + err.Error()})
			case !healed:
				log.Emit(migmon.Event{
					Kind:    migmon.EvCritical,
					Finding: migmon.FindingNoConvergence,
					Detail:  "a partition outlived its own heal; the failsafe sweep cleared it",
				})
				healed = true
			default:
				log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "failsafe sweep"})
			}
			repeer(log, clients)
		}
	}

	if !clk.sleepUntil(s.Quiet) {
		return false
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "schedule complete; disruptoor released"})
	return true
}

// holdUntilCrossed polls once a slot until every victim has crossed the fork on its own branch
// or HoldUntil passes; unreachable are victims whose probe failed on every poll, ok is false on shutdown.
func holdUntilCrossed(log *migmon.Log, o migsched.Op, probe crossedFn, slotSeconds int64, now func() time.Time, sleep func(time.Duration) bool) (crossed, unreachable map[int]bool, ok bool) {
	crossed = map[int]bool{}
	reached, warned := map[int]bool{}, map[int]bool{}
	paused := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var waiting []string
		for _, v := range o.Victims {
			if crossed[v] {
				continue
			}
			switch c, err := probe(ctx, v); {
			case err != nil:
				if !warned[v] {
					warned[v] = true
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: migsched.NodeName(v), Detail: "hold probe: " + err.Error()})
				}
			case c:
				reached[v], crossed[v] = true, true
				log.Emit(migmon.Event{Kind: migmon.EvIsolate, Node: migsched.NodeName(v), Detail: "crossed the fork on its own branch"})
				continue
			default:
				reached[v] = true
			}
			waiting = append(waiting, migsched.NodeName(v))
		}
		cancel()
		if len(waiting) == 0 || !now().Before(o.HoldUntil) {
			break
		}
		if !paused {
			paused = true
			log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "hold: waiting for " + strings.Join(waiting, ",") + " to cross the fork"})
		}
		if !sleep(time.Duration(slotSeconds) * time.Second) {
			return crossed, nil, false
		}
	}
	unreachable = map[int]bool{}
	for _, v := range o.Victims {
		if !reached[v] {
			unreachable[v] = true
		}
	}
	return crossed, unreachable, true
}

// sleepUntil returns false if a shutdown signal arrived first.
func sleepUntil(t time.Time, stop <-chan os.Signal) bool {
	d := time.Until(t)
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-stop:
		return false
	}
}

func window(o migsched.Op) string {
	return fmt.Sprintf("class=%s start=%d end=%d", o.Class, o.Start.Unix(), o.End.Unix())
}

// repeer rebuilds the peer mesh after a heal: a victim missing blocks otherwise has no peer to fetch from.
func repeer(log *migmon.Log, clients []migmon.Client) {
	if len(clients) < 2 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migmon.Repeer(ctx, clients); err != nil {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "re-peering after the heal: " + err.Error()})
		return
	}
	log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "execution clients re-peered"})
}
