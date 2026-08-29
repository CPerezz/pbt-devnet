package migmon

// Finding identifiers and thresholds shared by the monitor (emitter) and
// verify-migration (consumer). The numbers encode the acceptance contract
// the tools were calibrated against on live runs; they are not tunables.
const (
	// F1: two nodes report different non-null shadow roots for the SAME
	// block hash. CRITICAL, never waived - not by chaos windows, not by
	// phase.
	FindingRootMismatch = "F1"

	// F2: a direction reports stalled/error (immediate), or its cursor is
	// frozen while the node's head advances (slow-burn form below).
	FindingStall = "F2"

	// F3: boundary misbehaviour - the binary direction must park and the
	// merkle direction must start following within BoundaryPolls polls or
	// BoundaryBlocks blocks of the first header with time >= T; "done"
	// must come strictly after b*.
	FindingBoundary = "F3"

	// F4: a partition healed but the network did not converge - heads
	// still disagree well past the heal. This is the failure a
	// fork-straddling reorg risks: a node that cannot rewind across the
	// header-root format swap stays on its own branch forever, and the
	// run must fail loudly rather than wait for a verifier to notice.
	FindingNoConvergence = "F4"

	// Persistent-null sampling findings: a node keeps answering null for
	// sampled shadow roots while claiming following|synced.
	FindingNullWarn     = "NULL5"  // after NullWarnAfter
	FindingNullCritical = "NULL10" // after NullCriticalAfter
)

const (
	// StallPolls x head advance: cursor frozen for >= StallPolls polls
	// while the head advanced >= StallHeadDelta blocks -> CRITICAL.
	// Suspended while the direction is idle.
	StallPolls     = 20
	StallHeadDelta = 10

	// Boundary windows for F3.
	BoundaryPolls  = 2
	BoundaryBlocks = 3

	// Random-depth sampling: one probe per minute, uniform depth in
	// [SampleDepthMin, SampleDepthMax] behind the head.
	SampleDepthMin = 3
	SampleDepthMax = 16

	// Persistent-null escalation while following|synced.
	NullWarnAfterMinutes     = 5
	NullCriticalAfterMinutes = 10
)

const (
	// ConvergenceGrace is how long after a partition heals every node
	// must agree on a head again. Measured heals land a reorg within
	// seconds; two minutes is generous enough that only a genuinely
	// stuck node trips it.
	ConvergenceGrace = 120
)
