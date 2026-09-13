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
}

// Registry maps a client type to its ClientSpec, keyed by the short name in the service name.
var Registry = map[string]ClientSpec{
	"geth": {
		Introspects: true,
		// "Chain reorg detected number=39 ... drop=11 ...": number is the common ancestor.
		ReorgLogPattern:    `Chain reorg detected.*\bnumber=(?P<ancestor>\d+).*\bdrop=(?P<drop>\d+)`,
		ServesOrphans:      true,
		ForbiddenWindowLog: `migration window`,
	},
	"erigon": {
		Introspects: true,
		// "[4/8 Execution] Unwind Execution from=245 to=238": from is the head being
		// abandoned, to the unwind point, which is the common ancestor.
		ReorgLogPattern: `Unwind Execution.*\bfrom=(?P<from>\d+).*\bto=(?P<to>\d+)`,
		ServesOrphans:   true,
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
