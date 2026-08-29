package migsched

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Dump is the resolved schedule as it appears in the chaos driver's JSONL,
// once, at startup. It is the only place the schedule crosses a process
// boundary: the acceptance verifier reads it to learn each op's class and
// therefore which heal deadline applies, and the lap driver reads it to
// place host-side work (a node restart) in a gap no partition occupies.
// Publishing it beats both sides re-deriving a schedule they could get
// wrong differently.
type Dump struct {
	Profile   string   `json:"profile"`
	Genesis   int64    `json:"genesis"`
	Fork      int64    `json:"fork"`
	Heavy     int      `json:"heavy"`
	Ops       []DumpOp `json:"ops"`
	Failsafes []int64  `json:"failsafes"`
	Quiet     int64    `json:"quiet"`
}

// DumpOp is one op in the dump. Times are unix seconds: the JSONL already
// carries RFC3339 event times, and integers here keep the arithmetic
// consumers do (windows, waivers, gaps) free of parsing.
type DumpOp struct {
	Name    string `json:"name"`
	Class   string `json:"class"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	Victims []int  `json:"victims"`
	Refused string `json:"refused,omitempty"`
}

// NewDump renders the schedule for publication.
func (s Schedule) NewDump(genesis, fork time.Time, heavy int) Dump {
	d := Dump{
		Profile: s.Profile,
		Genesis: genesis.Unix(),
		Fork:    fork.Unix(),
		Heavy:   heavy,
		Quiet:   s.Quiet.Unix(),
	}
	for _, o := range s.Ops {
		d.Ops = append(d.Ops, DumpOp{
			Name:    o.Name,
			Class:   string(o.Class),
			Start:   o.Start.Unix(),
			End:     o.End.Unix(),
			Victims: o.Victims,
			Refused: o.Refused,
		})
	}
	for _, f := range s.Failsafes {
		d.Failsafes = append(d.Failsafes, f.Unix())
	}
	return d
}

// ParseDump reads a published schedule.
func ParseDump(raw []byte) (Dump, error) {
	var d Dump
	if err := json.Unmarshal(raw, &d); err != nil {
		return Dump{}, fmt.Errorf("schedule dump: %w", err)
	}
	if d.Profile == "" || d.Fork == 0 {
		return Dump{}, fmt.Errorf("schedule dump carries no profile or fork time: %s", raw)
	}
	return d, nil
}

// Admitted returns the ops that will actually run.
func (d Dump) Admitted() []DumpOp {
	var out []DumpOp
	for _, o := range d.Ops {
		if o.Refused == "" {
			out = append(out, o)
		}
	}
	return out
}

// HealDeadline returns the instant by which op must have been healed, per
// its class: a pre-fork op by the time the network has to be whole again for
// the approach to the fork, the straddle by its own end plus the convergence
// allowance. A verifier applying one global deadline would fail every
// straddle for healing after the fork, which is what a straddle does.
//
// A pre-fork op's deadline is deliberately NOT its own scheduled end. The
// heal is applied after sleeping to that end, so its recorded time is always
// a moment later, and an op whose end coincides with a sweep instant could
// never pass. The requirement is that the network is whole before the fork
// approach, so the last sweep at or before the fork is the deadline.
func (d Dump) HealDeadline(o DumpOp) time.Time {
	if Class(o.Class) == ClassStraddle {
		return time.Unix(o.End, 0).Add(straddleExclusiveAfter * time.Second)
	}
	fork := time.Unix(d.Fork, 0)
	deadline := fork
	for _, f := range d.Failsafes {
		if t := time.Unix(f, 0); !t.After(fork) && t.After(deadline) || deadline.Equal(fork) && !t.After(fork) {
			deadline = t
		}
	}
	return deadline
}

// Gaps returns the intervals inside [from, to] that no admitted partition
// occupies. The lap driver places its node restart in one of these and
// refuses to run if its intended window is not contained in a gap: a
// restart during a partition would confuse a heal with a reboot.
func (d Dump) Gaps(from, to time.Time) [][2]time.Time {
	busy := make([][2]time.Time, 0, len(d.Ops))
	for _, o := range d.Admitted() {
		busy = append(busy, [2]time.Time{time.Unix(o.Start, 0), time.Unix(o.End, 0)})
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i][0].Before(busy[j][0]) })

	var out [][2]time.Time
	cursor := from
	for _, b := range busy {
		if !b[1].After(cursor) {
			continue // entirely behind the cursor
		}
		if b[0].After(cursor) {
			end := b[0]
			if end.After(to) {
				end = to
			}
			if end.After(cursor) {
				out = append(out, [2]time.Time{cursor, end})
			}
		}
		if b[1].After(cursor) {
			cursor = b[1]
		}
		if !cursor.Before(to) {
			return out
		}
	}
	if cursor.Before(to) {
		out = append(out, [2]time.Time{cursor, to})
	}
	return out
}

// Contains reports whether [start, end] fits inside one gap.
func Contains(gaps [][2]time.Time, start, end time.Time) bool {
	for _, g := range gaps {
		if !start.Before(g[0]) && !end.After(g[1]) {
			return true
		}
	}
	return false
}
