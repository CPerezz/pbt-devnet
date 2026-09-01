// Package migsched resolves the partition schedule a migrating devnet runs
// against. It is shared: the chaos driver executes the schedule, and the
// post-migration gate needs the same instants to know when a cross-node
// divergence is legal chaos and when it is a wedge worth healing. Both
// import this package rather than passing the numbers between services,
// so the two can never drift apart.
//
// Every schedule is resolved once, at startup, from the genesis time, the
// fork time and the topology. Nothing observed at runtime changes it. The
// only dynamic rule is refusal: an op that would still be open when the
// network has to be whole again is skipped loudly, never compressed.
package migsched

import (
	"fmt"
	"sort"
	"time"
)

// Class names what an op is for. It travels in the JSONL so the acceptance
// verifier can apply the right heal deadline to each op instead of one
// global rule.
type Class string

const (
	// ClassDeep isolates the stake-heavy victim long enough to build a
	// deep branch. Its share stalls finality for the window, which is
	// what keeps the heal reorg survivable.
	ClassDeep Class = "deep"
	// ClassShort isolates a light victim: the connected majority keeps
	// finalizing, and the heal reorg is shallow and quick.
	ClassShort Class = "short"
	// ClassStraddle spans the fork time itself - both sides cross it
	// independently, on different blocks, and the victim rewinds across
	// the header-root format swap when the partition heals.
	ClassStraddle Class = "straddle"
	// ClassWindow isolates a light victim inside the open migration
	// window, after the fork: the format swap must tolerate chaos while
	// it is live, not just on either side of it.
	ClassWindow Class = "window"
)

// Anchor says what an op's offsets are measured from.
type Anchor int

const (
	// FromGenesis anchors an op to block 0's timestamp.
	FromGenesis Anchor = iota
	// FromFork anchors an op to the tree activation time, so a straddle
	// keeps its geometry whatever offset the profile runs.
	FromFork
)

// Op is one resolved partition window.
type Op struct {
	Name    string
	Class   Class
	Start   time.Time
	End     time.Time
	Victims []int  // participant indices, 1-based, as disruptoor selectors take them
	Refused string // non-empty when admission rejected the op; it will not run
}

// Admitted reports whether the op will actually be applied.
func (o Op) Admitted() bool { return o.Refused == "" }

// Schedule is a resolved profile.
type Schedule struct {
	Profile     string
	Ops         []Op
	Failsafes   []time.Time // unconditional Clear instants, ascending
	Quiet       time.Time   // after this the driver never disrupts again
	SlotSeconds int64       // seconds per slot, echoed into the dump for consumers doing slot math
}

// window is a profile entry before resolution.
type window struct {
	start, end int // seconds relative to the anchor
	anchor     Anchor
	class      Class
}

// profile is a named schedule plus the margin its pre-fork ops must clear.
type profile struct {
	windows []window
	// preForkMargin is how long before the fork every pre-fork partition
	// must be healed. Profiles that straddle the fork need the wider
	// margin so the quiet zone starts before the straddle's own window.
	preForkMargin int
}

// The shipped profiles. Deep windows are 190s because that is the scale a
// stake-heavy victim needs to reach depth >= 10 while remaining healable:
// longer windows let the majority finalize past the victim, and a victim
// whose branch conflicts with a finalized checkpoint is banned by the
// survivors and never rejoins. Shorts are 150s on light victims, the
// scale measured to heal cleanly with finality flowing.
var profiles = map[string]profile{
	// The full lifecycle: deep branches and a short before the fork, a
	// gap for a node restart, a partition straddling the fork, then a
	// light isolation inside the open migration window. The gap between
	// the short and the quiet zone is deliberate - the lap driver
	// restarts a light node there, and that must not overlap any
	// partition.
	"composite": {
		windows: []window{
			{start: 240, end: 430, anchor: FromGenesis, class: ClassDeep},
			{start: 540, end: 730, anchor: FromGenesis, class: ClassDeep},
			{start: 790, end: 940, anchor: FromGenesis, class: ClassShort},
			{start: -90, end: 90, anchor: FromFork, class: ClassStraddle},
			{start: 240, end: 390, anchor: FromFork, class: ClassWindow},
		},
		preForkMargin: 390,
	},
	// All three phases, small: one light short, then the straddle.
	"composite-smoke": {
		windows: []window{
			{start: 220, end: 370, anchor: FromGenesis, class: ClassShort},
			{start: -90, end: 90, anchor: FromFork, class: ClassStraddle},
		},
		preForkMargin: 390,
	},
}

// Names lists the known profiles, for flag validation and error messages.
func Names() []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	return out
}

const (
	// straddleMinSide is how much of the straddle must fall on each side
	// of the fork: both islands need time to mint their own first block
	// past it, or there is no format swap to rewind across.
	straddleMinSide = 60
	// straddleMaxAfter bounds how long the network may stay split past
	// the fork.
	straddleMaxAfter = 120
	// healConvergeAllowance is how long a post-fork op's heal reorg may
	// take to converge before the verifier calls the network wedged.
	healConvergeAllowance = 120
	// legalDivergenceTail extends each op's legal-divergence window past
	// its end: the heal reorg is still propagating through the survivors,
	// so divergence there is expected, not a fault.
	legalDivergenceTail = 120
	// quietTail keeps the driver's Clear authority this long past the
	// last post-fork op, so the repeat sweep lands before permanent quiet.
	quietTail = 120
	// straddleMaxDrop bounds the victims' expected dropped-branch length,
	// keeping the heal rewind far inside the state a node retains.
	straddleMaxDrop = 24
	// failsafeRepeat is the gap before a post-fork op's idempotent second
	// heal: the first one failing must not leave the network split.
	failsafeRepeat = 60

	// windowOpenAfter is the earliest a window op may start. The straddle
	// may stay split until fork+120s and its heal is allowed 120s more to
	// converge, so the migration window is only provably calm from here.
	windowOpenAfter = 240
	// windowCloseBy is the latest a window op may end, keeping the whole
	// disruption inside the span the migration window is guaranteed open.
	windowCloseBy = 420
	// windowMinLen and windowMaxLen bound a window op: shorter builds no
	// observable divergence, longer approaches the deep-op scale a light
	// victim cannot heal from once the majority finalizes past it.
	windowMinLen = 60
	windowMaxLen = 180
)

// Topology is the participant layout a schedule resolves against.
type Topology struct {
	Heavy          int     // the stake-heavy participant, victim of deep and straddle ops
	Lights         []int   // disruptable light participants, in rotation order
	HeavyShare     float64 // heavy's share of the validator set, 0..1
	SecondsPerSlot int
}

// share returns a participant's stake share. Topology records only the
// heavy participant's share; the remainder is assumed to split evenly
// across the lights plus the bootnode, which holds one light-sized slice.
func (t Topology) share(participant int) float64 {
	if participant == t.Heavy {
		return t.HeavyShare
	}
	return (1 - t.HeavyShare) / float64(len(t.Lights)+1)
}

// Resolve turns a profile name and a topology into a schedule.
func Resolve(name string, genesis, fork time.Time, topo Topology) (Schedule, error) {
	p, ok := profiles[name]
	if !ok {
		return Schedule{}, fmt.Errorf("unknown profile %q, want one of %v", name, Names())
	}
	if topo.Heavy <= 0 {
		return Schedule{}, fmt.Errorf("no heavy participant given: deep and straddle ops have no valid victim")
	}
	if len(topo.Lights) == 0 {
		return Schedule{}, fmt.Errorf("no light participants given: every node is protected or heavy")
	}
	if topo.SecondsPerSlot <= 0 {
		return Schedule{}, fmt.Errorf("seconds-per-slot must be positive, got %d", topo.SecondsPerSlot)
	}

	s := Schedule{Profile: name, SlotSeconds: int64(topo.SecondsPerSlot)}
	deadline := fork.Add(-time.Duration(p.preForkMargin) * time.Second)
	rotation := 0
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
		switch w.class {
		case ClassDeep:
			o.Victims = []int{topo.Heavy}
		case ClassStraddle:
			o.Victims = []int{topo.Heavy}
		case ClassShort, ClassWindow:
			o.Victims = []int{topo.Lights[rotation%len(topo.Lights)]}
			rotation++
		default:
			return Schedule{}, fmt.Errorf("op %d has unknown class %q", i+1, w.class)
		}

		switch w.class {
		case ClassStraddle:
			straddles++
			if straddles > 1 {
				return Schedule{}, fmt.Errorf("profile %q resolves %d straddles; exactly one is allowed", name, straddles)
			}
			if err := admitStraddle(o, fork, topo); err != nil {
				return Schedule{}, err
			}
		case ClassWindow:
			if err := admitWindow(o, fork, topo); err != nil {
				return Schedule{}, err
			}
		default:
			if o.End.After(deadline) {
				o.Refused = fmt.Sprintf("%s op ends %s, after the pre-fork heal deadline %s (fork-%ds)",
					w.class, o.End.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339), p.preForkMargin)
			}
		}
		s.Ops = append(s.Ops, o)
	}

	// Exclusivity around the straddle needs no separate check: every
	// pre-fork op is refused unless it ends by the deadline, which sits
	// before the straddle opens, and a window op may not start before the
	// straddle's heal allowance has fully run out.
	// TestProfilesRespectStraddleWindow holds that invariant over the
	// shipped profiles.
	s.Failsafes, s.Quiet = failsafes(s.Ops, deadline)
	if err := checkFailsafes(s.Ops, s.Failsafes); err != nil {
		return Schedule{}, err
	}
	return s, nil
}

// admitStraddle enforces the straddle's own rules. They are resolve-time
// errors, not refusals: a straddle profile whose fork geometry does not fit
// is a misconfiguration, and running it minus its only interesting op would
// look like a pass.
func admitStraddle(o Op, fork time.Time, topo Topology) error {
	var share float64
	for _, v := range o.Victims {
		share += topo.share(v)
	}
	if share <= 1.0/3 || share >= 0.5 {
		return fmt.Errorf("straddle isolates a %.2f stake share, need inside (1/3, 1/2): at or below 1/3 the majority finalizes past the victims mid-window and bans their branch, at or above 1/2 the island can finalize a branch the heal cannot rewind", share)
	}
	before := fork.Sub(o.Start)
	after := o.End.Sub(fork)
	if before < straddleMinSide*time.Second || after < straddleMinSide*time.Second {
		return fmt.Errorf("straddle spans %.0fs before and %.0fs after the fork; each side needs at least %ds for both branches to cross it",
			before.Seconds(), after.Seconds(), straddleMinSide)
	}
	if after > straddleMaxAfter*time.Second {
		return fmt.Errorf("straddle stays split %.0fs past the fork, more than the %ds bound", after.Seconds(), straddleMaxAfter)
	}
	slots := int(o.End.Sub(o.Start).Seconds()) / topo.SecondsPerSlot
	if drop := float64(slots) * share; drop > straddleMaxDrop {
		return fmt.Errorf("straddle would drop about %.0f blocks (%d slots at a %.2f summed share), over the %d bound",
			drop, slots, share, straddleMaxDrop)
	}
	return nil
}

// admitWindow enforces the window op's rules. Like the straddle's, they
// are resolve-time errors, not refusals: a profile that cannot place its
// window op inside the open migration window is a misconfiguration, and
// running it without the op would look like a pass.
func admitWindow(o Op, fork time.Time, topo Topology) error {
	if len(o.Victims) != 1 || o.Victims[0] == topo.Heavy {
		return fmt.Errorf("window victim is %v, must be a single light participant: isolating the heavy share mid-migration stalls finality while the format swap is live", o.Victims)
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

// checkFailsafes rejects a schedule whose sweep instants fall inside an
// admitted op's window: an unconditional Clear there tears the partition
// down mid-op and silently destroys the evidence the op exists to produce.
// A sweep exactly at an op's end is legal - the heal runs first (Actions
// orders op-end before a same-instant sweep) and the sweep backstops it.
func checkFailsafes(ops []Op, sweeps []time.Time) error {
	for _, f := range sweeps {
		for _, o := range ops {
			if !o.Admitted() {
				continue
			}
			if !f.Before(o.Start) && f.Before(o.End) {
				return fmt.Errorf("failsafe sweep at %s falls inside op %s [%s,%s)",
					f.UTC().Format(time.RFC3339), o.Name,
					o.Start.UTC().Format(time.RFC3339), o.End.UTC().Format(time.RFC3339))
			}
		}
	}
	return nil
}

// failsafes returns the unconditional Clear instants and the moment the
// driver goes quiet for good. Bookkeeping is not a safety mechanism: every
// instant here fires a Clear whatever the driver thinks it has already
// healed, so a crashed or confused run cannot leave the network split.
//
// The pre-fork deadline always gets a sweep, and the last admitted pre-fork
// op gets an earlier one when it ends before that: the early sweep closes
// partitions promptly, the deadline sweep is the one that guarantees the
// network is whole for the approach to the fork even if everything between
// them failed. The straddle gets a sweep at its end plus a repeat a minute
// later; a window op's heal fires at its own end, so it gets the repeat
// alone.
func failsafes(ops []Op, deadline time.Time) ([]time.Time, time.Time) {
	var (
		out     []time.Time
		lastPre time.Time
	)
	quiet := deadline
	for _, o := range ops {
		switch o.Class {
		case ClassStraddle:
			out = append(out, o.End, o.End.Add(failsafeRepeat*time.Second))
		case ClassWindow:
			out = append(out, o.End.Add(failsafeRepeat*time.Second))
		default:
			if o.Admitted() && o.End.After(lastPre) {
				lastPre = o.End
			}
			continue
		}
		if q := o.End.Add(quietTail * time.Second); q.After(quiet) {
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

// LegalWindows returns the intervals during which cross-node head
// divergence is expected chaos rather than a fault: each admitted op's
// window, extended to cover the heal and the convergence that follows it.
// The gate's watchdog uses these to avoid healing a partition the schedule
// is deliberately holding.
func (s Schedule) LegalWindows() [][2]time.Time {
	var out [][2]time.Time
	for _, o := range s.Ops {
		if !o.Admitted() {
			continue
		}
		out = append(out, [2]time.Time{o.Start, o.End.Add(legalDivergenceTail * time.Second)})
	}
	return out
}

// Covers reports whether t falls inside any legal-divergence window.
func (s Schedule) Covers(t time.Time) bool {
	for _, w := range s.LegalWindows() {
		if !t.Before(w[0]) && !t.After(w[1]) {
			return true
		}
	}
	return false
}

// Straddle returns the straddle op, if the profile has one.
func (s Schedule) Straddle() (Op, bool) {
	for _, o := range s.Ops {
		if o.Class == ClassStraddle {
			return o, true
		}
	}
	return Op{}, false
}

// ActionKind tags one entry in the serial action list.
type ActionKind int

// Kinds sort by value when two actions share an instant: the heal must
// land before the sweep that backstops it, and both before any new
// isolation.
const (
	ActOpEnd ActionKind = iota
	ActSweep
	ActOpStart
)

// Action is one instant of driver work. The driver executes the action
// list as a single serial loop; running ops and sweeps as separate passes
// fires the sweeps bundled after the last op instead of at their scheduled
// instants.
type Action struct {
	Kind ActionKind
	At   time.Time
	Op   Op // the op being started or ended; zero for sweeps
}

// StaleAt reports whether executing the action at now is pointless: an op
// whose window has already closed must be skipped, never run as a
// zero-length isolate/heal. Op ends and sweeps are never stale - a Clear
// is idempotent, and firing one late is strictly safer than not firing it.
func (a Action) StaleAt(now time.Time) bool {
	return a.Kind == ActOpStart && !a.Op.End.After(now)
}

// Actions flattens the schedule into one chronologically sorted list.
// Refused ops do not appear: they never run, and the driver announces
// them from Ops before the loop starts.
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
