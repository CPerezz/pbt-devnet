// Command migration-chaos applies a fixed partition schedule to a
// migrating devnet: deep windows on the stake-heavy node before the fork,
// short ones on the light nodes, and - in the profiles that ask for it -
// one partition spanning the fork itself, so both sides cross it on
// different blocks and the victim has to rewind across the header-root
// format swap when the network heals.
//
// The schedule is resolved by internal/migsched, which the post-migration
// gate also imports, so the two services agree on every instant without
// passing numbers between them. This command owns disruptoor from genesis
// until the schedule goes quiet; nothing else may touch it in that window.
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

// elFlag collects repeated --el name=url flags. The URL goes unused here
// (partitions are applied by participant index, through disruptoor's own
// selectors), but the FLAG ORDER is load-bearing: position i, 1-based, is
// the participant index, exactly as the package renders the list.
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
		profile   = flag.String("profile", "", "schedule profile: "+strings.Join(migsched.Names(), ", "))
		heavy     = flag.Int("heavy-node", 0, "participant index holding the heavy validator share")
		share     = flag.Float64("heavy-share", 0.40, "that participant's share of the validator set")
		slotSecs  = flag.Int("seconds-per-slot", 6, "chain slot duration")
		dryRun    = flag.Bool("dry-run", false, "print the resolved schedule and exit")
		jsonlPath = flag.String("jsonl", "", "JSONL event path (default stdout)")
		keys      cli.MultiFlag
	)
	flag.Var(&keys, "key", "hex private key of a prefunded account for straddle state injection (repeatable; 2+ enables the injector)")
	flag.Var(&els, "el", "execution client as name=url, repeatable; order is the participant index")
	flag.Var(&protected, "protect-node", "participant index never isolated, repeatable")
	flag.Parse()

	if *genesisTS == 0 || *forkTS == 0 || *profile == "" || len(els.names) == 0 || *heavy == 0 {
		fmt.Fprintln(os.Stderr, "required: --el (>=1), --genesis-time, --binary-trie-time, --profile, --heavy-node")
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
	topo, err := topology(els.names, protected.vals, *heavy, *share, *slotSecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "topology: %v\n", err)
		os.Exit(2)
	}
	sched, err := migsched.Resolve(*profile, genesis, fork, topo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedule: %v\n", err)
		os.Exit(2)
	}

	// Publish the resolved schedule before doing anything with it: the
	// acceptance verifier reads it for each op's class and heal deadline,
	// and the lap driver reads it to place host-side work in a gap.
	publish(log, sched, genesis, fork, *heavy)

	if *dryRun {
		emitPlan(log, sched)
		return
	}
	if *api == "" {
		fmt.Fprintln(os.Stderr, "required outside --dry-run: --disruptoor")
		os.Exit(2)
	}

	d := disruptoor.New(*api, 15*time.Second)
	// A selector matching nothing is accepted, changes no traffic and
	// leaves a healthy chain behind - the failure indistinguishable from
	// success. Refuse to start instead.
	n, err := d.Containers()
	if err != nil || n == 0 {
		fmt.Fprintf(os.Stderr, "disruptoor sees %d containers (err=%v); refusing to run no-op chaos\n", n, err)
		os.Exit(1)
	}

	// The execution clients are needed only to rebuild their peer mesh
	// after a partition; nothing here reads chain state.
	clients := make([]migmon.Client, 0, len(els.names))
	for i, name := range els.names {
		clients = append(clients, migmon.NewClient(name, els.urls[i]))
	}

	// The straddle injector needs one door into each island: the victim's
	// own RPC (participant indices are 1-based flag order) and a protected
	// node's RPC, which is majority-side by construction. Fewer than two
	// keys, or no straddle in this profile, leaves it off - the schedule
	// runs identically, minus the engineered writes.
	var inj *injector
	if str, ok := sched.Straddle(); ok && len(keys) >= 2 {
		majority := 1
		if len(protected.vals) > 0 {
			majority = protected.vals[0]
		}
		victimIdx, majIdx := str.Victims[0]-1, majority-1
		if victimIdx >= 0 && victimIdx < len(els.urls) && majIdx >= 0 && majIdx < len(els.urls) {
			parsed := make([]*ecdsa.PrivateKey, 0, len(keys))
			for _, k := range keys {
				key, err := crypto.HexToECDSA(strings.TrimPrefix(k, "0x"))
				if err != nil {
					fmt.Fprintf(os.Stderr, "bad --key: %v\n", err)
					os.Exit(2)
				}
				parsed = append(parsed, key)
			}
			var err error
			if inj, err = newInjector(els.urls[victimIdx], els.urls[majIdx], parsed); err != nil {
				fmt.Fprintf(os.Stderr, "injector: %v\n", err)
				os.Exit(2)
			}
		}
	}
	run(log, d, sched, len(els.names), clients, inj)
}

// topology derives the victim layout from the flags: the heavy participant
// takes every deep and straddle window, the remaining unprotected nodes
// rotate through the short ones.
func topology(names []string, protect []int, heavy int, share float64, slotSecs int) (migsched.Topology, error) {
	blocked := map[int]bool{}
	for _, n := range protect {
		blocked[n] = true
	}
	if blocked[heavy] {
		return migsched.Topology{}, fmt.Errorf("participant %d is both the heavy victim and protected", heavy)
	}
	if heavy < 1 || heavy > len(names) {
		return migsched.Topology{}, fmt.Errorf("heavy participant %d is outside the %d configured clients", heavy, len(names))
	}
	var lights []int
	for i := range names {
		idx := i + 1
		if idx != heavy && !blocked[idx] {
			lights = append(lights, idx)
		}
	}
	return migsched.Topology{
		Heavy:          heavy,
		Lights:         lights,
		HeavyShare:     share,
		SecondsPerSlot: slotSecs,
	}, nil
}

// publish writes the resolved schedule as one JSONL record.
func publish(log *migmon.Log, s migsched.Schedule, genesis, fork time.Time, heavy int) {
	raw, err := json.Marshal(s.NewDump(genesis, fork, heavy))
	if err != nil {
		// A schedule that cannot be published cannot be judged either.
		fmt.Fprintf(os.Stderr, "publishing the schedule: %v\n", err)
		os.Exit(1)
	}
	log.Emit(migmon.Event{Kind: migmon.EvSchedule, Detail: s.Profile, Raw: raw})
}

// emitPlan prints what the schedule would do, without touching disruptoor.
// Plan records carry plan=true so a reader can never mistake a dry-run's
// isolate lines for partitions that actually happened.
func emitPlan(log *migmon.Log, s migsched.Schedule) {
	for _, o := range s.Ops {
		kind, detail := migmon.EvIsolate, window(o)
		if !o.Admitted() {
			kind, detail = migmon.EvSkip, o.Refused
		}
		for _, v := range o.Victims {
			log.Emit(migmon.Event{Kind: kind, Node: nodeName(v), Detail: detail, Plan: true})
		}
	}
	for _, f := range s.Failsafes {
		log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "sweep at " + f.UTC().Format(time.RFC3339), Plan: true})
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "quiet from " + s.Quiet.UTC().Format(time.RFC3339), Plan: true})
}

// run executes the schedule as ONE serial action list: op starts, op ends
// and failsafe sweeps interleaved in wall-clock order. Running ops and
// sweeps as separate passes bundles every sweep after the last op, which
// live runs proved fires them minutes late - harmless while every heal
// works, and exactly wrong the day one does not.
func run(log *migmon.Log, d *disruptoor.Client, s migsched.Schedule, participants int, clients []migmon.Client, inj *injector) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for _, o := range s.Ops {
		if !o.Admitted() {
			for _, v := range o.Victims {
				log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: nodeName(v), Detail: o.Refused})
			}
		}
	}

	// Tracks whether the last op's own heal is known to have worked; a
	// failsafe sweep that finds it false announces the rescue loudly.
	healed := true

	for _, a := range s.Actions() {
		switch a.Kind {
		case migsched.ActOpStart:
			// The injection contract deploys shortly before the straddle
			// opens: the network is still whole, so the deploy is plainly
			// canonical, and close enough that ambient reorgs cannot age it.
			if inj != nil && a.Op.Class == migsched.ClassStraddle {
				if !sleepUntil(a.At.Add(-60*time.Second), stop) {
					d.Clear()
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
				if err := inj.deploy(ctx, log); err != nil {
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "injection deploy: " + err.Error()})
				}
				cancel()
			}
			if !sleepUntil(a.At, stop) {
				d.Clear()
				return
			}
			if a.StaleAt(time.Now()) {
				log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: nodeName(a.Op.Victims[0]),
					Detail: fmt.Sprintf("op %s window already closed at execution time; skipped, not run zero-length", a.Op.Name)})
				continue
			}
			if err := d.Partition(a.Op.Name, others(participants, a.Op.Victims), a.Op.Victims); err != nil {
				for _, v := range a.Op.Victims {
					log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: nodeName(v), Detail: "partition failed: " + err.Error()})
				}
				continue
			}
			healed = false
			for _, v := range a.Op.Victims {
				log.Emit(migmon.Event{Kind: migmon.EvIsolate, Node: nodeName(v), Detail: window(a.Op)})
			}
			if inj != nil && a.Op.Class == migsched.ClassStraddle {
				// Let the islands mint their first separate blocks, then
				// write the conflicting state through each island's door.
				if !sleepUntil(a.At.Add(15*time.Second), stop) {
					d.Clear()
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
				inj.splitWrites(ctx, log)
				cancel()
			}

		case migsched.ActOpEnd:
			if !sleepUntil(a.At, stop) {
				d.Clear()
				return
			}
			if err := d.Clear(); err != nil {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "heal failed: " + err.Error()})
				continue
			}
			healed = true
			for _, v := range a.Op.Victims {
				log.Emit(migmon.Event{Kind: migmon.EvHeal, Node: nodeName(v), Detail: "window closed"})
			}
			repeer(log, clients)

		case migsched.ActSweep:
			if !sleepUntil(a.At, stop) {
				d.Clear()
				return
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

	if !sleepUntil(s.Quiet, stop) {
		return
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "schedule complete; disruptoor released"})
	<-stop
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

// others returns every participant index outside victims, ascending.
func others(participants int, victims []int) []int {
	skip := map[int]bool{}
	for _, v := range victims {
		skip[v] = true
	}
	var out []int
	for i := 1; i <= participants; i++ {
		if !skip[i] {
			out = append(out, i)
		}
	}
	return out
}

func nodeName(idx int) string { return fmt.Sprintf("node-%d", idx) }

func window(o migsched.Op) string {
	return fmt.Sprintf("class=%s start=%d end=%d", o.Class, o.Start.Unix(), o.End.Unix())
}

// repeer rebuilds the execution layer's peer mesh after a partition.
// Without it a victim that missed blocks cannot get them: its consensus
// client points it at a head it does not have, and the execution client has
// no peers to fetch the gap from, so it sits at its old head for good.
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
