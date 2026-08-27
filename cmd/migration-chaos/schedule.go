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

// profileWindows are genesis-relative offsets in seconds. The r3 numbers
// are the plan's: deep-first so the randao dice get two full 12-minute
// tries at depth >= 10, then a paired short island that books the two
// remaining healed-isolation counts inside one window.
var profiles = map[string][]struct {
	start, end int
	nVictims   int
	deep       bool
}{
	"smoke": {
		{start: 360, end: 540, nVictims: 1, deep: false},
	},
	"r3": {
		{start: 240, end: 960, nVictims: 1, deep: true},
		{start: 1140, end: 1860, nVictims: 1, deep: true},
		{start: 1890, end: 2040, nVictims: 2, deep: false},
	},
}

// chaosMarginSeconds is how long before T every partition must be healed:
// the boundary must be crossed by a connected network (plan A8/E).
const chaosMarginSeconds = 300

// resolve builds the schedule. eligible are participant indices that may
// be isolated (protected nodes already removed), in rotation order.
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
	for i, w := range windows {
		o := op{
			name:  fmt.Sprintf("mig-%s-%d", profile, i+1),
			start: genesis.Add(time.Duration(w.start) * time.Second),
			end:   genesis.Add(time.Duration(w.end) * time.Second),
			deep:  w.deep,
		}
		for range w.nVictims {
			o.victims = append(o.victims, eligible[rotation%len(eligible)])
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
