package migmon

// The offline lap (args/migration-offline.yaml): main.star prints the plan as
// pbt_offline=<json>, cmd/migration-swap runs it and writes the records, the
// monitor draws each step, and verify-migration reads plan and records back
// from the lap manifest.

// Offline is the pbt_migration.offline block, plus the validating count main.star adds.
type Offline struct {
	Enabled            bool              `json:"enabled"`
	ExportAfterSeconds int64             `json:"export_after_seconds"`
	MarginSeconds      int64             `json:"margin_seconds"`
	Producers          []OfflineProducer `json:"producers"`
	Consumers          []OfflineConsumer `json:"consumers"` // swapped in this order
	OnlineOnly         []OnlineOnly      `json:"online_only"`
	Validating         int               `json:"validating"` // validators sit on participants 1..Validating
}

type OfflineProducer struct {
	Node      int    `json:"node"`
	Kind      string `json:"kind"` // geth-convert | erigon-export
	Consumers []int  `json:"consumers"`
}

type OfflineConsumer struct {
	Node            int   `json:"node"`
	ExpectedSeconds int64 `json:"expected_seconds"`
	TimeoutSeconds  int64 `json:"timeout_seconds"`
}

// OnlineOnly is a participant with no importer for another client's artifacts, and why.
type OnlineOnly struct {
	Node   int    `json:"node"`
	Reason string `json:"reason"`
}

// ProducerRecord is one export. Times are unix seconds, 0 = never happened; Dir is
// relative to the lap's output directory.
type ProducerRecord struct {
	Node            int               `json:"node"`
	Kind            string            `json:"kind"`
	Status          string            `json:"status"` // ok | failed
	Detail          string            `json:"detail"`
	Anchor          uint64            `json:"anchor"`
	AnchorHash      string            `json:"anchor_hash"`
	AnchorStateRoot string            `json:"anchor_state_root"`
	PbtRoot         string            `json:"pbt_root"`
	ShadowRoots     map[string]string `json:"shadow_roots"` // EL service -> debug_shadowStateRoot(anchor_hash), "" = none
	StartedAt       int64             `json:"started_at"`
	ExportedAt      int64             `json:"exported_at"`
	DownFrom        int64             `json:"down_from"` // the producer's own downtime; 0 for a live export
	DownTo          int64             `json:"down_to"`
	Dir             string            `json:"dir"`
	Consumers       []int             `json:"consumers"`
}

// SwapRecord is one consumer's stop, import, restart and replay.
type SwapRecord struct {
	Node            int    `json:"node"`
	Producer        int    `json:"producer"`
	Anchor          uint64 `json:"anchor"`
	AnchorHash      string `json:"anchor_hash"`
	Status          string `json:"status"` // ok | failed | timeout | skipped
	Detail          string `json:"detail"`
	FinalizedAtStop uint64 `json:"finalized_at_stop"` // node 1's finalized height just before the stop
	StopAt          int64  `json:"stop_at"`
	StartAt         int64  `json:"start_at"`
	ReachableAt     int64  `json:"reachable_at"`
	CaughtUpAt      int64  `json:"caught_up_at"`
	EvidenceDir     string `json:"evidence_dir"`
}

// Lifecycle is POST /api/lifecycle's body: Node moved to Step (anchor/hash set once known).
type Lifecycle struct {
	Node   int    `json:"node"`
	Step   string `json:"step"`
	Anchor uint64 `json:"anchor,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// CaughtUpLag: a consumer whose binary cursor is this close to its head has caught up.
const CaughtUpLag = 2

// Lifecycle steps: POST /api/lifecycle's step and EvLifecycle's Phase.
const (
	StepExporting    = "exporting" // producer steps
	StepExported     = "exported"
	StepExportFailed = "export_failed"

	StepPending       = "pending" // consumer steps
	StepDisconnecting = "disconnecting"
	StepImporting     = "importing"
	StepReconnecting  = "reconnecting"
	StepReplaying     = "replaying"
	StepCaughtUp      = "caught_up"
	StepFailed        = "failed"
	StepTimeout       = "timeout"
	StepSkipped       = "skipped"
	StepNoImporter    = "no_importer"
)
