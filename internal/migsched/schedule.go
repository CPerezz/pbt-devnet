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
	Profile   string
	Ops       []Op
	Failsafes []time.Time // unconditional Clear instants, ascending
	Quiet     time.Time   // after this the driver never disrupts again
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
	// One short isolation, far from the fork: proves the machinery.
	"smoke": {
		windows:       []window{{start: 360, end: 540, anchor: FromGenesis, class: ClassShort}},
		preForkMargin: 300,
	},
	// Pre-fork acceptance: three tries at a deep branch, then two shorts
	// rotating over the light nodes.
	"full": {
		windows: []window{
			{start: 240, end: 430, anchor: FromGenesis, class: ClassDeep},
			{start: 540, end: 730, anchor: FromGenesis, class: ClassDeep},
			{start: 840, end: 1030, anchor: FromGenesis, class: ClassDeep},
			{start: 1090, end: 1240, anchor: FromGenesis, class: ClassShort},
			{start: 1300, end: 1450, anchor: FromGenesis, class: ClassShort},
		},
		preForkMargin: 300,
	},
	// The full lifecycle: deep branches and a short before the fork, a
	// gap for a node restart, then a partition straddling the fork. The
	// gap between the short and the quiet zone is deliberate - the lap
	// driver restarts a light node there, and that must not overlap any
	// partition.
	"composite": {
		windows: []window{
			{start: 240, end: 430, anchor: FromGenesis, class: ClassDeep},
			{start: 540, end: 730, anchor: FromGenesis, class: ClassDeep},
			{start: 790, end: 940, anchor: FromGenesis, class: ClassShort},
			{start: -90, end: 90, anchor: FromFork, class: ClassStraddle},
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
	// The straddle alone, for iterating on the boundary itself.
	"straddle-smoke": {
		windows:       []window{{start: -90, end: 90, anchor: FromFork, class: ClassStraddle}},
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
	// straddleExclusiveAfter keeps every other op clear of the straddle's
	// heal and the convergence that follows it.
	straddleExclusiveAfter = 120
	// straddleMaxDrop bounds the victim's expected dropped-branch length,
	// keeping the heal rewind far inside the state a node retains.
	straddleMaxDrop = 24
	// failsafeRepeat is the gap before the straddle's idempotent second
	// heal: the first one failing must not leave the network split.
	failsafeRepeat = 60
)

// Topology is the participant layout a schedule resolves against.
type Topology struct {
	Heavy          int     // the stake-heavy participant, victim of deep and straddle ops
	Lights         []int   // disruptable light participants, in rotation order
	HeavyShare     float64 // heavy's share of the validator set, 0..1
	SecondsPerSlot int
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

	s := Schedule{Profile: name}
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
		case ClassDeep, ClassStraddle:
			o.Victims = []int{topo.Heavy}
		case ClassShort:
			o.Victims = []int{topo.Lights[rotation%len(topo.Lights)]}
			rotation++
		default:
			return Schedule{}, fmt.Errorf("op %d has unknown class %q", i+1, w.class)
		}

		if w.class == ClassStraddle {
			straddles++
			if straddles > 1 {
				return Schedule{}, fmt.Errorf("profile %q resolves %d straddles; exactly one is allowed", name, straddles)
			}
			if err := admitStraddle(o, fork, topo); err != nil {
				return Schedule{}, err
			}
		} else if o.End.After(deadline) {
			o.Refused = fmt.Sprintf("%s op ends %s, after the pre-fork heal deadline %s (fork-%ds)",
				w.class, o.End.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339), p.preForkMargin)
		}
		s.Ops = append(s.Ops, o)
	}

	// Exclusivity around the straddle needs no separate check: every
	// pre-fork op is refused unless it ends by the deadline, which sits
	// before the straddle opens, so an admitted non-straddle op cannot
	// reach into the straddle's window. TestProfilesRespectStraddleWindow
	// holds that invariant over the shipped profiles.
	s.Failsafes, s.Quiet = failsafes(s.Ops, deadline)
	return s, nil
}

// admitStraddle enforces the straddle's own rules. They are resolve-time
// errors, not refusals: a straddle profile whose fork geometry does not fit
// is a misconfiguration, and running it minus its only interesting op would
// look like a pass.
func admitStraddle(o Op, fork time.Time, topo Topology) error {
	if len(o.Victims) != 1 || o.Victims[0] != topo.Heavy {
		return fmt.Errorf("straddle victim is %v, must be the stake-heavy participant %d: a light victim lets the majority finalize past it mid-window", o.Victims, topo.Heavy)
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
	if drop := float64(slots) * topo.HeavyShare; drop > straddleMaxDrop {
		return fmt.Errorf("straddle would drop about %.0f blocks (%d slots at a %.2f share), over the %d bound",
			drop, slots, topo.HeavyShare, straddleMaxDrop)
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
// them failed.
func failsafes(ops []Op, deadline time.Time) ([]time.Time, time.Time) {
	var (
		out      []time.Time
		straddle *Op
		lastPre  time.Time
	)
	for i := range ops {
		o := ops[i]
		if o.Class == ClassStraddle {
			straddle = &ops[i]
			continue
		}
		if o.Admitted() && o.End.After(lastPre) {
			lastPre = o.End
		}
	}
	if !lastPre.IsZero() && lastPre.Before(deadline) {
		out = append(out, lastPre)
	}
	out = append(out, deadline)
	if straddle == nil {
		return out, deadline
	}
	out = append(out, straddle.End, straddle.End.Add(failsafeRepeat*time.Second))
	return out, straddle.End.Add(straddleExclusiveAfter * time.Second)
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
		out = append(out, [2]time.Time{o.Start, o.End.Add(straddleExclusiveAfter * time.Second)})
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
