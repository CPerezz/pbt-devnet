// Package migsched resolves the partition schedule a migrating devnet runs,
// shared by the chaos driver, the gate and the monitor; an op that would still
// be open when the network must be whole is refused, never compressed.
package migsched

import (
	"fmt"
	"sort"
	"time"
)

// Class names what an op is for; it travels in the JSONL so each op gets its own heal deadline.
type Class string

const (
	ClassDeep     Class = "deep"     // a pair of lights (>1/3: stalls finality, cannot win) builds a deep branch
	ClassShort    Class = "short"    // one light, while the majority keeps finalizing
	ClassStraddle Class = "straddle" // every light on its own island across I*: each crosses on its own block and rewinds over the swap
	ClassWindow   Class = "window"   // one light inside the open migration window
)

// Anchor says what an op's offsets are measured from: genesis or the fork.
type Anchor int

const (
	FromGenesis Anchor = iota
	FromFork
)

// Op is one resolved partition window.
type Op struct {
	Name      string
	Class     Class
	Start     time.Time
	End       time.Time
	Victims   []int     // 1-based participant indices, as disruptoor selectors take them
	Mutual    bool      // each victim its own island; else one island against the rest
	Refused   string    // why admission rejected the op; empty when it runs
	HoldUntil time.Time // latest heal: the driver heals at End once every island crossed the fork, here regardless
}

func (o Op) Admitted() bool { return o.Refused == "" }

// Latest is the instant the op is certainly healed.
func (o Op) Latest() time.Time {
	if !o.HoldUntil.IsZero() {
		return o.HoldUntil
	}
	return o.End
}

// Schedule is a resolved profile.
type Schedule struct {
	Profile     string
	Ops         []Op
	Failsafes   []time.Time // unconditional Clear instants, ascending
	Quiet       time.Time   // after this the driver never disrupts again
	SlotSeconds int64
}

type window struct {
	start, end int // seconds relative to the anchor
	anchor     Anchor
	class      Class
}

type profile struct {
	windows       []window
	preForkMargin int // every pre-fork partition is healed this long before the fork
}

// Deep windows are 190s: what a 40% island needs for depth >= 10 while still
// healable. Shorts are 150s: measured to heal cleanly with finality flowing.
// The lap restarts a light node at +1080, after the composite short's heal
// (+940) and its 120s convergence allowance, before the straddle's setup.
var profiles = map[string]profile{
	"composite": {
		windows: []window{
			{start: 240, end: 430, anchor: FromGenesis, class: ClassDeep},
			{start: 540, end: 730, anchor: FromGenesis, class: ClassDeep},
			{start: 790, end: 940, anchor: FromGenesis, class: ClassShort},
			{start: -120, end: 60, anchor: FromFork, class: ClassStraddle},
			{start: 300, end: 450, anchor: FromFork, class: ClassWindow},
		},
		preForkMargin: 390,
	},
	"composite-smoke": {
		windows: []window{
			{start: 220, end: 370, anchor: FromGenesis, class: ClassShort},
			{start: -120, end: 60, anchor: FromFork, class: ClassStraddle},
		},
		preForkMargin: 390,
	},
}

func Names() []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

const (
	straddleMinSide       = 60   // seconds each side of the fork needs for every island to mint past it
	straddleMaxHold       = 120  // how long past End the driver waits for a slow island to cross
	straddleMaxAfter      = 180  // longest split past the fork, hold included
	straddleMargin        = 0.05 // anchor must outweigh each island by this: proposer boost is 1.25% (mainnet preset), 5% (minimal)
	healConvergeAllowance = 120  // a post-fork heal reorg may take this long before the network counts as wedged
	legalDivergenceTail   = 120  // divergence stays legal this long past an op for heal propagation
	quietTail             = 120  // Clear authority kept this long past the last post-fork op for the repeat sweep
	straddleMaxDrop       = 24   // bound on one island's expected dropped branch
	failsafeRepeat        = 60   // gap before a post-fork op's idempotent second heal
	windowOpenAfter       = 300  // earliest window op: straddleMaxAfter + healConvergeAllowance
	windowCloseBy         = 480  // latest window-op end, inside the span the migration window is guaranteed open
	windowMinLen          = 60   // shorter shows no divergence
	windowMaxLen          = 180  // longer nears deep scale
)

// Topology is the participant layout. The anchor holds the heavy stake and is
// never a victim: the lighter side of a heal rewinds, so every light is forced
// back onto the anchor's chain - across the fork too - and the anchor never is.
type Topology struct {
	Anchor         int
	Lights         []int // disruptable participants, in rotation order
	Participants   int   // protected ones included; sets the light share
	AnchorShare    float64
	SecondsPerSlot int
}

// share is a participant's stake; the non-anchor remainder splits evenly.
func (t Topology) share(participant int) float64 {
	if participant == t.Anchor {
		return t.AnchorShare
	}
	return (1 - t.AnchorShare) / float64(t.Participants-1)
}

// Lights lists the disruptable participants: everyone but the anchor and the protected.
func Lights(participants, anchor int, protect []int) []int {
	blocked := map[int]bool{anchor: true}
	for _, n := range protect {
		blocked[n] = true
	}
	var out []int
	for i := 1; i <= participants; i++ {
		if !blocked[i] {
			out = append(out, i)
		}
	}
	return out
}

// Others lists every participant outside victims, ascending.
func Others(participants int, victims []int) []int {
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

// NodeName is how the chaos and gate streams name a participant.
func NodeName(participant int) string { return fmt.Sprintf("node-%d", participant) }

func Resolve(name string, genesis, fork time.Time, topo Topology) (Schedule, error) {
	p, ok := profiles[name]
	if !ok {
		return Schedule{}, fmt.Errorf("unknown profile %q, want one of %v", name, Names())
	}
	if topo.Anchor <= 0 {
		return Schedule{}, fmt.Errorf("no anchor participant given: no chain for the heals to converge on")
	}
	if len(topo.Lights) == 0 {
		return Schedule{}, fmt.Errorf("no light participants given: every node is protected or the anchor")
	}
	for _, l := range topo.Lights {
		if l == topo.Anchor {
			return Schedule{}, fmt.Errorf("participant %d is both the anchor and a light", l)
		}
	}
	if topo.Participants < len(topo.Lights)+1 {
		return Schedule{}, fmt.Errorf("%d participants cannot hold an anchor and %d lights", topo.Participants, len(topo.Lights))
	}
	if topo.SecondsPerSlot <= 0 {
		return Schedule{}, fmt.Errorf("seconds-per-slot must be positive, got %d", topo.SecondsPerSlot)
	}

	s := Schedule{Profile: name, SlotSeconds: int64(topo.SecondsPerSlot)}
	deadline := fork.Add(-time.Duration(p.preForkMargin) * time.Second)
	rotation, pairs := 0, 0
	straddles := 0

	for i, w := range p.windows {
		base := genesis
		if w.anchor == FromFork {
			base = fork
		}
		o := Op{
			Name:  fmt.Sprintf("mig-%s-%d", name, i+1),
			Class: w.class,
			Start: base.Add(time.Duration(w.start) * time.Second),
			End:   base.Add(time.Duration(w.end) * time.Second),
		}
		var err error
		switch w.class {
		case ClassDeep:
			// Consecutive lights in rotation order; with three lights the pairs cycle.
			if len(topo.Lights) < 2 {
				return Schedule{}, fmt.Errorf("op %d is a deep partition but only %d light is disruptable; a deep island is a pair", i+1, len(topo.Lights))
			}
			n := len(topo.Lights)
			o.Victims = []int{topo.Lights[pairs%n], topo.Lights[(pairs+1)%n]}
			pairs++
			err = admitDeep(o, topo)
		case ClassStraddle:
			o.Victims = append([]int(nil), topo.Lights...)
			o.Mutual = true
			o.HoldUntil = o.End.Add(straddleMaxHold * time.Second)
			if straddles++; straddles > 1 {
				return Schedule{}, fmt.Errorf("profile %q resolves %d straddles; exactly one is allowed", name, straddles)
			}
			err = admitStraddle(o, fork, topo)
		case ClassShort, ClassWindow:
			o.Victims = []int{topo.Lights[rotation%len(topo.Lights)]}
			rotation++
			if w.class == ClassWindow {
				err = admitWindow(o, fork, topo)
			}
		default:
			return Schedule{}, fmt.Errorf("op %d has unknown class %q", i+1, w.class)
		}
		if err != nil {
			return Schedule{}, err
		}
		// Pre-fork ops must be healed before the margin; a late one is refused, never compressed.
		if (w.class == ClassDeep || w.class == ClassShort) && o.End.After(deadline) {
			o.Refused = fmt.Sprintf("%s op ends %s, after the pre-fork heal deadline %s (fork-%ds)",
				w.class, o.End.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339), p.preForkMargin)
		}
		s.Ops = append(s.Ops, o)
	}

	s.Failsafes, s.Quiet = failsafes(s.Ops, deadline)
	if err := checkFailsafes(s.Ops, s.Failsafes); err != nil {
		return Schedule{}, err
	}
	return s, nil
}

// admitDeep: the island must stall finality without being able to win the heal.
func admitDeep(o Op, topo Topology) error {
	var share float64
	for _, v := range o.Victims {
		share += topo.share(v)
	}
	if share <= 1.0/3 || share >= 0.5 {
		return fmt.Errorf("deep island %v holds a %.2f stake share, need inside (1/3, 1/2): at or below 1/3 the majority finalizes past it mid-window and bans the branch, at or above 1/2 it can finalize a branch the heal cannot rewind", o.Victims, share)
	}
	return nil
}

// admitStraddle: every island must lose to the anchor with room for proposer
// boost, and no side may reach 2/3 while split or it finalizes a fork block the
// rest can never rewind.
func admitStraddle(o Op, fork time.Time, topo Topology) error {
	if !o.Mutual || len(o.Victims) == 0 {
		return fmt.Errorf("straddle must isolate every light on its own island, got victims %v mutual=%v", o.Victims, o.Mutual)
	}
	anchor := topo.share(topo.Anchor)
	if anchor >= 2.0/3 {
		return fmt.Errorf("anchor holds a %.2f stake share, at or above 2/3 it finalizes its own fork block mid-straddle and closes the window early", anchor)
	}
	worst := 0.0
	for _, v := range o.Victims {
		if s := topo.share(v); s > worst {
			worst = s
		}
	}
	if anchor < worst+straddleMargin {
		return fmt.Errorf("anchor share %.2f does not outweigh a %.2f island by the %.2f proposer-boost margin: that island could win the heal and never rewind across the fork", anchor, worst, straddleMargin)
	}
	before := fork.Sub(o.Start)
	after := o.End.Sub(fork)
	if before < straddleMinSide*time.Second || after < straddleMinSide*time.Second {
		return fmt.Errorf("straddle spans %.0fs before and %.0fs after the fork; each side needs at least %ds for every island to cross it",
			before.Seconds(), after.Seconds(), straddleMinSide)
	}
	if hold := o.Latest().Sub(fork); hold > straddleMaxAfter*time.Second {
		return fmt.Errorf("straddle may stay split %.0fs past the fork, more than the %ds bound", hold.Seconds(), straddleMaxAfter)
	}
	slots := int(o.Latest().Sub(o.Start).Seconds()) / topo.SecondsPerSlot
	if drop := float64(slots) * worst; drop > straddleMaxDrop {
		return fmt.Errorf("an island would drop about %.0f blocks (%d slots at a %.2f share), over the %d bound",
			drop, slots, worst, straddleMaxDrop)
	}
	return nil
}

func admitWindow(o Op, fork time.Time, topo Topology) error {
	if len(o.Victims) != 1 || o.Victims[0] == topo.Anchor {
		return fmt.Errorf("window victim is %v, must be a single light participant: isolating the anchor mid-migration stalls finality while the format swap is live", o.Victims)
	}
	length := o.End.Sub(o.Start)
	if length < windowMinLen*time.Second || length > windowMaxLen*time.Second {
		return fmt.Errorf("window op runs %.0fs, want %d..%ds", length.Seconds(), windowMinLen, windowMaxLen)
	}
	if o.Start.Before(fork.Add(windowOpenAfter * time.Second)) {
		return fmt.Errorf("window op starts %s, before the migration window is provably calm (fork+%ds)",
			o.Start.UTC().Format(time.RFC3339), windowOpenAfter)
	}
	if o.End.After(fork.Add(windowCloseBy * time.Second)) {
		return fmt.Errorf("window op ends %s, past the span the migration window is guaranteed open (fork+%ds)",
			o.End.UTC().Format(time.RFC3339), windowCloseBy)
	}
	return nil
}

// checkFailsafes: a sweep inside an admitted window would heal it early; one at
// its end is legal, the heal runs first.
func checkFailsafes(ops []Op, sweeps []time.Time) error {
	for _, f := range sweeps {
		for _, o := range ops {
			if !o.Admitted() {
				continue
			}
			if !f.Before(o.Start) && f.Before(o.Latest()) {
				return fmt.Errorf("failsafe sweep at %s falls inside op %s [%s,%s)",
					f.UTC().Format(time.RFC3339), o.Name,
					o.Start.UTC().Format(time.RFC3339), o.Latest().UTC().Format(time.RFC3339))
			}
		}
	}
	return nil
}

// failsafes: the pre-fork deadline always sweeps, the last pre-fork op sweeps
// earlier when it ends before that, the straddle at its latest heal plus a
// repeat, a window op the repeat alone.
func failsafes(ops []Op, deadline time.Time) ([]time.Time, time.Time) {
	var (
		out     []time.Time
		lastPre time.Time
	)
	quiet := deadline
	for _, o := range ops {
		switch o.Class {
		case ClassStraddle:
			out = append(out, o.Latest(), o.Latest().Add(failsafeRepeat*time.Second))
		case ClassWindow:
			out = append(out, o.End.Add(failsafeRepeat*time.Second))
		default:
			if o.Admitted() && o.End.After(lastPre) {
				lastPre = o.End
			}
			continue
		}
		if q := o.Latest().Add(quietTail * time.Second); q.After(quiet) {
			quiet = q
		}
	}
	if !lastPre.IsZero() && lastPre.Before(deadline) {
		out = append(out, lastPre)
	}
	out = append(out, deadline)
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, quiet
}

// LegalWindows: when head divergence is expected chaos, each admitted op plus the tail.
func (s Schedule) LegalWindows() [][2]time.Time {
	var out [][2]time.Time
	for _, o := range s.Ops {
		if !o.Admitted() {
			continue
		}
		out = append(out, [2]time.Time{o.Start, o.Latest().Add(legalDivergenceTail * time.Second)})
	}
	return out
}

func (s Schedule) Covers(t time.Time) bool {
	for _, w := range s.LegalWindows() {
		if !t.Before(w[0]) && !t.After(w[1]) {
			return true
		}
	}
	return false
}

func (s Schedule) Straddle() (Op, bool) {
	for _, o := range s.Ops {
		if o.Class == ClassStraddle {
			return o, true
		}
	}
	return Op{}, false
}

// ActionKind orders actions sharing an instant: heal, then sweep, then isolation.
type ActionKind int

const (
	ActOpEnd ActionKind = iota
	ActSweep
	ActOpStart
)

// Action is one instant of driver work.
type Action struct {
	Kind ActionKind
	At   time.Time
	Op   Op // the op being started or ended; zero for sweeps
}

// StaleAt: an op start whose window already closed is skipped, never run zero-length.
func (a Action) StaleAt(now time.Time) bool {
	return a.Kind == ActOpStart && !a.Op.End.After(now)
}

// Actions flattens the schedule into one sorted list; refused ops do not appear.
func (s Schedule) Actions() []Action {
	out := make([]Action, 0, 2*len(s.Ops)+len(s.Failsafes))
	for _, o := range s.Ops {
		if !o.Admitted() {
			continue
		}
		out = append(out,
			Action{Kind: ActOpStart, At: o.Start, Op: o},
			Action{Kind: ActOpEnd, At: o.End, Op: o},
		)
	}
	for _, f := range s.Failsafes {
		out = append(out, Action{Kind: ActSweep, At: f})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
