package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// elFlag collects repeated --el name=url flags. The URL is unused by this
// driver (partitions are p2p, applied via disruptoor selectors), but the
// FLAG ORDER is load-bearing: position i (1-based) is the participant
// index disruptoor addresses, exactly as main.star renders the list.
type elFlag struct{ names []string }

func (e *elFlag) String() string { return strings.Join(e.names, ",") }
func (e *elFlag) Set(v string) error {
	name, _, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=url, got %q", v)
	}
	e.names = append(e.names, name)
	return nil
}

type intsFlag struct{ vals []int }

func (f *intsFlag) String() string { return fmt.Sprint(f.vals) }
func (f *intsFlag) Set(v string) error {
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fmt.Errorf("not a node index: %q", v)
	}
	f.vals = append(f.vals, n)
	return nil
}

func main() {
	var (
		els       elFlag
		protected intsFlag
		api       = flag.String("disruptoor", "", "disruptoor native API base URL")
		genesisTS = flag.Int64("genesis-time", 0, "chain genesis unix time (schedule anchor)")
		forkTS    = flag.Int64("binary-trie-time", 0, "fork time T; all chaos ends by T-300s")
		profile   = flag.String("profile", "", "schedule profile: smoke or r3")
		dryRun    = flag.Bool("dry-run", false, "print the resolved schedule as JSONL and exit")
		jsonlPath = flag.String("jsonl", "", "JSONL event path (default stdout)")
	)
	flag.Var(&els, "el", "execution client as name=url, repeatable; order = participant index")
	flag.Var(&protected, "protect-node", "participant index never isolated, repeatable")
	flag.Parse()

	if *genesisTS == 0 || *forkTS == 0 || *profile == "" || len(els.names) == 0 {
		fmt.Fprintln(os.Stderr, "required: --el (>=1), --genesis-time, --binary-trie-time, --profile")
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

	isProtected := map[int]bool{}
	for _, n := range protected.vals {
		isProtected[n] = true
	}
	var eligible []int
	for i := range els.names {
		if idx := i + 1; !isProtected[idx] {
			eligible = append(eligible, idx)
		}
	}
	sched, err := resolve(*profile, time.Unix(*genesisTS, 0), time.Unix(*forkTS, 0), eligible)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedule: %v\n", err)
		os.Exit(2)
	}

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
	// success. Refuse to start instead (same preflight as pbtchaos).
	n, err := d.Containers()
	if err != nil || n == 0 {
		fmt.Fprintf(os.Stderr, "disruptoor sees %d containers (err=%v); refusing to run no-op chaos\n", n, err)
		os.Exit(1)
	}

	run(log, d, sched, len(els.names))
}

// emitPlan prints the resolved schedule without touching disruptoor.
func emitPlan(log *migmon.Log, s schedule) {
	for _, o := range s.ops {
		for _, v := range o.victims {
			kind, detail := migmon.EvIsolate, window(o)
			if o.refused != "" {
				kind, detail = migmon.EvSkip, o.refused
			}
			log.Emit(migmon.Event{Kind: kind, Node: nodeName(v), Detail: detail})
		}
	}
	log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "forced heal-all at " + s.healAll.UTC().Format(time.RFC3339)})
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "quiet from " + s.quiet.UTC().Format(time.RFC3339)})
}

// run executes the schedule in wall-clock time. Ops are strictly serial by
// construction (resolve never overlaps windows); each ends with a global
// Clear because disruptoor state is global - the reason the plan pairs the
// two short isolations into one window instead of overlapping them.
func run(log *migmon.Log, d *disruptoor.Client, s schedule, participants int) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for _, o := range s.ops {
		if o.refused != "" {
			for _, v := range o.victims {
				log.Emit(migmon.Event{Kind: migmon.EvSkip, Node: nodeName(v), Detail: o.refused})
			}
			continue
		}
		if !sleepUntil(o.start, stop) {
			return
		}
		majority := others(participants, o.victims)
		if err := d.Partition(o.name, majority, o.victims); err != nil {
			for _, v := range o.victims {
				log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: nodeName(v), Detail: "partition failed: " + err.Error()})
			}
			continue
		}
		for _, v := range o.victims {
			log.Emit(migmon.Event{Kind: migmon.EvIsolate, Node: nodeName(v), Detail: window(o)})
		}
		if !sleepUntil(o.end, stop) {
			// Dying mid-partition would leave the network split for
			// good; heal before honouring the signal.
			d.Clear()
			return
		}
		if err := d.Clear(); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "heal failed: " + err.Error()})
			continue
		}
		for _, v := range o.victims {
			log.Emit(migmon.Event{Kind: migmon.EvHeal, Node: nodeName(v), Detail: "window closed"})
		}
	}

	// The forced heal is a backstop, not bookkeeping: even if every op
	// already cleared itself, one more Clear is harmless and guarantees
	// the boundary is crossed connected.
	if sleepUntil(s.healAll, stop) {
		if err := d.Clear(); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: "forced heal failed: " + err.Error()})
		} else {
			log.Emit(migmon.Event{Kind: migmon.EvHeal, Detail: "forced heal-all (schedule backstop)"})
		}
	} else {
		return
	}
	if !sleepUntil(s.quiet, stop) {
		return
	}
	log.Emit(migmon.Event{Kind: migmon.EvPause, Detail: "T-300 reached; schedule permanently quiet"})
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
	sort.Ints(out)
	return out
}

func nodeName(idx int) string { return fmt.Sprintf("node-%d", idx) }

func window(o op) string {
	return fmt.Sprintf("start=%d end=%d deep=%v", o.start.Unix(), o.end.Unix(), o.deep)
}
