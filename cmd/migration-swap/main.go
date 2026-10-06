// Command migration-swap drives the offline EIP-8347 migration lap: two producers export
// real PBT artifacts from a finalized block, two consumers take turns importing them and
// replaying block access lists to head, strictly one at a time inside chaos-free gaps.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/disruptoor"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

var sayMu sync.Mutex

func say(format string, args ...any) {
	sayMu.Lock()
	defer sayMu.Unlock()
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migration-swap: "+format+"\n", args...)
	os.Exit(1)
}

// opLock creates $OUT/swap.lock while at least one producer or consumer is mid-operation,
// and removes it when the last one finishes; the two producers run concurrently, so this
// is a counter, not a single flag.
type opLock struct {
	mu   sync.Mutex
	n    int
	path string
}

func (l *opLock) begin() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	if l.n == 1 {
		os.WriteFile(l.path, nil, 0o644)
	}
}

func (l *opLock) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.n == 0 {
		os.Remove(l.path)
	}
}

type runner struct {
	enc           string
	cfg           migmon.Offline
	nodes         map[int]node
	services      map[string]bool
	total         int // every participant the monitor sees
	validating    int // participants 1..validating: what the chaos schedule and the swap gates see
	dump          migsched.Dump
	genesis, fork int64
	out           string
	fault         string
	ev            *Evidence
	lock          *opLock

	monitorURL string
	disrupt    *disruptoor.Client

	elClients map[int]migmon.Client
	clURLs    map[int]string
}

// partitions counts the partitions disruptoor holds: 0 when the enclave has no
// disruptoor, -1 when it does and cannot be read.
func (r *runner) partitions() int {
	if r.disrupt == nil {
		return 0
	}
	n, _, err := r.disrupt.State()
	if err != nil {
		return -1
	}
	return n
}

// init resolves every host URL and RPC client this run needs before anything else can run.
func (r *runner) init() error {
	var err error
	if r.monitorURL, err = r.portURL("migration-monitor", "http"); err != nil {
		return fmt.Errorf("resolving monitor url: %w", err)
	}
	// No disruptoor (chaos_profile: none) means nothing can be partitioned.
	if r.services["disruptoor"] {
		disruptoorURL, err := r.portURL("disruptoor", "http")
		if err != nil {
			return fmt.Errorf("resolving disruptoor url: %w", err)
		}
		r.disrupt = disruptoor.New(disruptoorURL, 10*time.Second)
	}

	r.elClients = make(map[int]migmon.Client)
	r.clURLs = make(map[int]string)
	if err := r.resolveELs(); err != nil {
		return err
	}
	for i := 1; i <= r.validating; i++ {
		n := r.nodes[i]
		if n.CL == "" {
			continue
		}
		u, err := r.portURL(n.CL, "http")
		if err != nil {
			return fmt.Errorf("resolving %s url: %w", n.CL, err)
		}
		r.clURLs[i] = u
	}
	return nil
}

// resolveELs (re)builds every EL client; published ports change on every kurtosis service start.
func (r *runner) resolveELs() error {
	for i := 1; i <= r.total; i++ {
		n := r.nodes[i]
		u, err := r.portURL(n.EL, "rpc")
		if err != nil {
			return fmt.Errorf("resolving %s url: %w", n.EL, err)
		}
		r.elClients[i] = migmon.NewClient(n.EL, u)
	}
	return nil
}

func (r *runner) producer(node int) *migmon.ProducerRecord {
	for _, p := range r.ev.Producers {
		if p.Node == node {
			return p
		}
	}
	return nil
}

func (r *runner) swap(node int) *migmon.SwapRecord {
	for _, s := range r.ev.Swaps {
		if s.Node == node {
			return s
		}
	}
	return nil
}

func writeEvidence(out string, ev *Evidence) {
	b, err := ev.marshal()
	if err != nil {
		say("marshaling swaps.json: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(out, "swaps.json"), b, 0o644); err != nil {
		say("writing swaps.json: %v", err)
	}
}

func parseOffline(raw string) (migmon.Offline, error) {
	var cfg migmon.Offline
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("parsing: %w", err)
	}
	if !cfg.Enabled {
		return cfg, fmt.Errorf("enabled is false")
	}
	if len(cfg.Producers) == 0 || len(cfg.Consumers) == 0 {
		return cfg, fmt.Errorf("no producers or consumers")
	}
	return cfg, nil
}

// printPlan resolves exactly what a real run would (services, schedule) and prints the
// timeline without touching any client or disruptoor.
func printPlan(cfg migmon.Offline, nodes map[int]node, dump migsched.Dump, genesis, fork int64) {
	notBefore := genesis + cfg.ExportAfterSeconds
	fmt.Printf("genesis=%d fork=%d export not before=%d (T+%ds)\n", genesis, fork, notBefore, cfg.ExportAfterSeconds)
	gaps := dump.Gaps(time.Unix(genesis, 0), time.Unix(fork, 0))
	fmt.Printf("chaos-free gaps before the fork: %d\n", len(gaps))
	for _, p := range cfg.Producers {
		fmt.Printf("  producer node=%d kind=%s el=%s consumers=%v\n", p.Node, p.Kind, nodes[p.Node].EL, p.Consumers)
	}
	for _, c := range cfg.Consumers {
		fmt.Printf("  swap node=%d el=%s expected=%ds timeout=%ds deadline: now+%ds+%ds<=fork(%d)\n",
			c.Node, nodes[c.Node].EL, c.ExpectedSeconds, c.TimeoutSeconds, c.ExpectedSeconds, cfg.MarginSeconds, fork)
	}
}

func main() {
	enclave := flag.String("enclave", "", "kurtosis enclave name")
	genesisTS := flag.Int64("genesis", 0, "chain genesis unix time")
	forkTS := flag.Int64("fork", 0, "binary-trie fork unix time")
	offlineJSON := flag.String("offline", "", "the pbt_offline JSON main.star prints (migmon.Offline)")
	outDir := flag.String("out", "", "evidence output directory")
	fault := flag.String("fault", "", "fault to inject: '' or corrupt-preimages (truncates the geth consumer's preimage file)")
	plan := flag.Bool("plan", false, "print the resolved timeline and exit without acting")
	flag.Parse()

	if *enclave == "" || *genesisTS == 0 || *forkTS == 0 || *offlineJSON == "" || *outDir == "" {
		die("required: --enclave --genesis --fork --offline --out")
	}
	if *forkTS <= *genesisTS {
		die("--fork (%d) must be after --genesis (%d)", *forkTS, *genesisTS)
	}
	if *fault != "" && *fault != "corrupt-preimages" {
		die("--fault must be '' or corrupt-preimages, got %q", *fault)
	}

	cfg, err := parseOffline(*offlineJSON)
	if err != nil {
		die("offline config: %v", err)
	}
	ev, err := newEvidence(cfg)
	if err != nil {
		die("offline config: %v", err)
	}
	nodes, services, err := resolveNodes(*enclave)
	if err != nil {
		die("%v", err)
	}
	dump, err := loadSchedule(*enclave, services)
	if err != nil {
		die("%v", err)
	}
	// Validators sit on participants 1..Validating (main.star puts observers last).
	validating := cfg.Validating
	if validating == 0 || validating > len(nodes) {
		validating = len(nodes)
	}

	if *plan {
		printPlan(cfg, nodes, dump, *genesisTS, *forkTS)
		return
	}

	if err := os.MkdirAll(filepath.Join(*outDir, "artifacts"), 0o755); err != nil {
		die("creating output dir: %v", err)
	}

	r := &runner{
		enc: *enclave, cfg: cfg, nodes: nodes, services: services, dump: dump,
		total: len(nodes), validating: validating,
		genesis: *genesisTS, fork: *forkTS, out: *outDir, fault: *fault,
		ev: ev, lock: &opLock{path: filepath.Join(*outDir, "swap.lock")},
	}
	if err := r.init(); err != nil {
		die("%v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		say("signalled; writing evidence and exiting")
		writeEvidence(r.out, r.ev)
		os.Exit(0)
	}()
	defer writeEvidence(r.out, r.ev)

	r.runProducers()
	r.runSwaps()
	say("done")
}
