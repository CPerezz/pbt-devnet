package migmon

import "time"

// Finding identifiers and thresholds shared by the monitor and verify-migration.
const (
	// root-mismatch: two nodes report different non-null shadow roots for the same block hash. CRITICAL, never waived.
	FindingRootMismatch = "root-mismatch"

	// stall: a direction reports stalled/error, or its cursor freezes while the head advances.
	FindingStall = "stall"

	// boundary: the binary direction must park and the merkle direction must
	// start following within BoundaryPolls polls or BoundaryBlocks blocks of I*; "done" must come strictly after I*.
	FindingBoundary = "boundary"

	// no-convergence: a partition healed but heads still disagree past ConvergenceGrace.
	FindingNoConvergence = "no-convergence"
	// A consensus client with zero peers outside any scheduled partition.
	FindingPeerStarved = "cl-starved"

	// Persistent-null: a node keeps answering null for sampled shadow roots while claiming following|synced.
	FindingNullWarn     = "null-warn"     // after NullWarnAfter
	FindingNullCritical = "null-critical" // after NullCriticalAfter
)

const (
	// StallPolls x head advance: cursor frozen >= StallPolls polls while head advanced >= StallHeadDelta blocks -> CRITICAL. Suspended while idle.
	StallPolls     = 20
	StallHeadDelta = 10

	// Boundary windows for boundary.
	BoundaryPolls  = 2
	BoundaryBlocks = 3

	// Random-depth sampling: one probe per minute, uniform depth in [SampleDepthMin, SampleDepthMax] behind the head.
	SampleDepthMin = 3
	SampleDepthMax = 16

	// Persistent-null escalation while following|synced.
	NullWarnAfterMinutes     = 5
	NullCriticalAfterMinutes = 10
)

const (
	// ConvergenceGrace: time after a partition heals for every node to agree on a head again.
	ConvergenceGrace = 120
)

// SplitGrace bounds how long nodes may disagree about the canonical chain
// before it counts as a fault. 12m: slowest legal recovery observed (a pair
// of victims rewinding across the format swap) with margin.
const SplitGrace = 12 * time.Minute

const (
	// IStarProvisionalGrace: how long nodes may disagree about the fork block
	// before it counts as a fault. 360s: longest schedule straddle window plus convergence allowance.
	IStarProvisionalGrace = 360
)
