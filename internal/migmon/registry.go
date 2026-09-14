package migmon

import "strings"

// ClientSpec is the client-onboarding evidence contract: what an execution
// client's implementation is expected to expose, so the monitor and
// verifier scope checks to what that client actually supports.
type ClientSpec struct {
	Introspects bool // exposes debug_migrationProgress and the shadow-root RPC
	// ReorgLogPattern matches the client's reorg log line, capturing either
	// "drop" (dropped-branch length) with an optional "ancestor" height, or
	// "from"+"to" (abandoned head and unwind point) to derive both. Empty
	// degrades that client's heals to monitor-events-only corroboration.
	ReorgLogPattern    string
	ServesOrphans      bool   // eth_getBlockByHash still answers for orphaned blocks
	ForbiddenWindowLog string // substring that must never appear during the forbidden window
	// RetiresShadow: once the fork block finalizes the client stops maintaining the
	// other tree, so a shadow root or an active direction after done is a regression.
	// A client that keeps both (erigon folds both commitment domains until an error
	// stops one) reports done with its shadow still live, and is not judged on it.
	RetiresShadow bool
}

// Registry maps a client type to its ClientSpec, keyed by the short name in the service name.
var Registry = map[string]ClientSpec{
	"geth": {
		Introspects: true,
		// "Chain reorg detected number=39 ... drop=11 ...": number is the common ancestor.
		ReorgLogPattern:    `Chain reorg detected.*\bnumber=(?P<ancestor>\d+).*\bdrop=(?P<drop>\d+)`,
		ServesOrphans:      true,
		ForbiddenWindowLog: `migration window`,
		RetiresShadow:      true,
	},
	"erigon": {
		Introspects: true,
		// "[4/8 Execution] Unwind Execution from=245 to=238": from is the head being
		// abandoned, to the unwind point, which is the common ancestor.
		ReorgLogPattern: `Unwind Execution.*\bfrom=(?P<from>\d+).*\bto=(?P<to>\d+)`,
		ServesOrphans:   true,
	},
	"besu": {
		// No debug_migrationProgress or debug_shadowStateRoot: besu swaps the trie per
		// header and has nothing to report about a background build.
		Introspects: false,
		// "Chain Reorganization +3 new / -2 old": the old-chain length is the branch
		// that was dropped. Logged only above --reorg-logging-threshold, which the
		// migration profiles set to 0.
		ReorgLogPattern: `Chain Reorganization \+\d+ new / -(?P<drop>\d+) old`,
		// Measured: besu still answered for an orphaned block the geth nodes had
		// already dropped. Safe in both directions - a client that stops serving makes
		// the parent walk error and fall back to its log.
		ServesOrphans: true,
	},
	// Same debug_migrationProgress / debug_shadowStateRoot wire shape as geth; its block
	// tree keeps every branch, so orphans stay readable by hash. No reorg log line carries
	// the depth, and no window log to forbid: corroboration comes from the monitor alone.
	// Measured: reports done when the fork block finalizes, with both directions and
	// the shadow root gone from then on.
	"nethermind": {
		Introspects:   true,
		ServesOrphans: true,
		RetiresShadow: true,
	},
}

// SpecFor returns the ClientSpec whose registry key appears as a substring
// of node, and whether one was found.
func SpecFor(node string) (ClientSpec, bool) {
	for key, spec := range Registry {
		if strings.Contains(node, key) {
			return spec, true
		}
	}
	return ClientSpec{}, false
}
