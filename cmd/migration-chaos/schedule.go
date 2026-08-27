// Command migration-chaos runs a FIXED partition schedule against a
// migrating devnet. Fixed means fixed: the ops, their windows and their
// victims are all resolved once at startup from the genesis time and the
// fork time T, and nothing observed at runtime changes them (plan A8). The
// one dynamic rule is refusal: an op that would still be open after T-300s
// is skipped loudly, never compressed, and after T-300s the driver goes
// permanently quiet so the boundary crossing is undisturbed.
package main

import (
	"fmt"
	"time"
)

// op is one partition window. Victims are participant indices (1-based,
// ethereum-package numbering, same ints disruptoor selectors take). Two
// victims mean one two-node minority island: disruptoor state is global,
// pbtchaos already serialises its own jobs for the same reason, so
// overlapping independent partitions are not a thing this driver does.
type op struct {
	name    string
	start   time.Time
	end     time.Time
	victims []int
	deep    bool // aimed at depth >= 10 (12-minute window)
	refused string
}

// schedule is the resolved plan: admitted ops in start order, a hard
// heal-all instant, and the moment the driver goes quiet forever.
type schedule struct {
	ops     []op
	healAll time.Time // forced Clear(), min(T-300, last admitted end)
	quiet   time.Time // T-300: after this, nothing but SIGTERM matters
}

// profileWindows are genesis-relative offsets in seconds, sized by two R3
// laps' worth of evidence. A partition that diverges across a finalized
// checkpoint strands the victim permanently: the MAJORITY side's peer
// scoring bans the victim's CL (a docker restart of the victim does not
// clear remote bans - proven live), and the legacy devnet's `make repeer`
// note documents the same pathology. So no window may let the majority
// finalize while the victim diverges. The r3 answer is stake, not
// duration: the deep victim (eligible[0], participant 2 in the migration
// args, 256 of 640 validators = 40%) leaves the connected majority at 60%
// - BELOW the 2/3 finality threshold - so finality stalls for the window
// instead of banning anyone, and a 40% proposer share yields depth >= 10
// inside a 190s window (~12.6 expected blocks, p~80% per try, ~99.2%
// across three) - the 3-minute scale S3 proved heals cleanly. Shorts
// rotate over the light nodes; their 80% connected majority keeps
// finality flowing and their heal reorgs are S3-shaped.
var profiles = map[string][]struct {
	start, end int
	deep       bool // deep ops pin eligible[0], the stake-heavy victim
}{
	"smoke": {
		{start: 360, end: 540, deep: false},
	},
	"r3": {
		{start: 240, end: 430, deep: true},
		{start: 540, end: 730, deep: true},
		{start: 840, end: 1030, deep: true},
		{start: 1090, end: 1240, deep: false},
		{start: 1300, end: 1450, deep: false},
	},
}

// chaosMarginSeconds is how long before T every partition must be healed:
// the boundary must be crossed by a connected network (plan A8/E).
const chaosMarginSeconds = 300

// resolve builds the schedule. eligible are participant indices that may
// be isolated (protected nodes already removed), in rotation order;
// eligible[0] is the stake-heavy deep victim by convention (the migration
// args files put the 256-validator participant first after the bootnode).
func resolve(profile string, genesis, forkTime time.Time, eligible []int) (schedule, error) {
	windows, ok := profiles[profile]
	if !ok {
		return schedule{}, fmt.Errorf("unknown profile %q", profile)
	}
	if len(eligible) == 0 {
		return schedule{}, fmt.Errorf("no eligible victims: every node is protected")
	}
	deadline := forkTime.Add(-chaosMarginSeconds * time.Second)

	var (
		s        = schedule{quiet: deadline}
		rotation = 0
		lastEnd  time.Time
	)
	// Shorts rotate over the light nodes so the heavy victim's windows
	// stay the only finality-stalling ones; with a single eligible node
	// everything lands on it.
	lights := eligible
	if len(eligible) > 1 {
		lights = eligible[1:]
	}
	for i, w := range windows {
		o := op{
			name:  fmt.Sprintf("mig-%s-%d", profile, i+1),
			start: genesis.Add(time.Duration(w.start) * time.Second),
			end:   genesis.Add(time.Duration(w.end) * time.Second),
			deep:  w.deep,
		}
		if w.deep {
			o.victims = []int{eligible[0]}
		} else {
			o.victims = []int{lights[rotation%len(lights)]}
			rotation++
		}
		// Admission: an op still open after the deadline is refused
		// outright. Compressing it instead would trade the plan's
		// stated windows for silently weaker chaos.
		if o.end.After(deadline) {
			o.refused = fmt.Sprintf("end %s is after heal deadline %s (T-%ds)",
				o.end.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339), chaosMarginSeconds)
		} else if o.end.After(lastEnd) {
			lastEnd = o.end
		}
		s.ops = append(s.ops, o)
	}
	s.healAll = deadline
	if !lastEnd.IsZero() && lastEnd.Before(deadline) {
		s.healAll = lastEnd
	}
	return s, nil
}
