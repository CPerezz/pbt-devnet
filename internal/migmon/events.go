package migmon

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Event kinds shared by the monitor's JSONL stream and the verify-migration reader.
const (
	EvProgress = "progress" // per-node debug_migrationProgress poll
	EvSample   = "sample"   // cross-node shadow-root sample at one block
	EvReorg    = "reorg"    // canonical hash changed under a sampled height
	EvIStar    = "istar"    // first header with time >= T, per node (provisional)
	EvHead     = "head"     // per-node head observation
	EvCritical = "critical" // a finding that fails the run
	EvWarn     = "warn"     // a finding that needs eyes, not failure

	// A reorg can orphan the recorded fork block; that observation is provisional until finalized.
	EvIStarReorged = "istar-reorged" // Detail=old->new, the recorded fork block was orphaned
	EvIStarFinal   = "istar-final"   // the node's fork block is finalized and settled

	// Chaos driver kinds, same stream shape, separate file.
	EvSchedule = "schedule" // resolved schedule, once at startup, Raw=migsched.Dump
	EvIsolate  = "isolate"  // Node=victim, Detail=window
	EvHeal     = "heal"     // Node=victim ("" = heal-all), Detail=reason
	EvPause    = "pause"    // the schedule went permanently quiet
	EvSkip     = "skip"     // an op was refused by the admission rule
	EvInject   = "inject"   // engineered state write around the straddle, Raw=Injection
)

// Injection is one engineered state write placed around the fork-straddling partition, checkable after the heal.
type Injection struct {
	Contract string `json:"contract"` // 0x address of the target contract
	Slot     string `json:"slot"`     // 0x storage slot key
	Value    string `json:"value"`    // 0x32-byte value this write set
	Side     string `json:"side"`     // "majority" | "victim": which island took the tx
	TxHash   string `json:"tx_hash"`
	// IslandBlock is the block hash that first included the victim-side tx; after heal it must be non-canonical everywhere.
	IslandBlock string `json:"island_block,omitempty"`
}

// Event is one JSONL line; fields are a union across kinds.
type Event struct {
	Time    time.Time         `json:"time"`
	Kind    string            `json:"kind"`
	Node    string            `json:"node,omitempty"`
	Number  uint64            `json:"number,omitempty"`
	Hash    string            `json:"hash,omitempty"`
	Roots   map[string]string `json:"roots,omitempty"`   // node -> shadow root ("" = null)
	Phase   string            `json:"phase,omitempty"`   // progress: top-level phase
	Detail  string            `json:"detail,omitempty"`  // human line
	Finding string            `json:"finding,omitempty"` // root-mismatch, stall, boundary, null-warn, null-critical, ...
	Raw     json.RawMessage   `json:"raw,omitempty"`     // progress: verbatim RPC result
	Plan    bool              `json:"plan,omitempty"`    // dry-run: this isolate/skip was never executed
}

// Log serialises events to one writer, one JSON object per line.
type Log struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewLog(w io.Writer) *Log { return &Log{enc: json.NewEncoder(w)} }

// Emit writes one event, stamping the time if unset.
func (l *Log) Emit(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(ev)
}
