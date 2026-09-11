package migmon

import "strings"

// ClientSpec is the client-onboarding evidence contract: what an execution
// client's implementation is expected to expose, so the monitor and
// verifier scope checks to what that client actually supports.
type ClientSpec struct {
	Introspects        bool   // exposes debug_migrationProgress and the shadow-root RPC
	ReorgLogPattern    string // substring a reorg notice logs
	ServesOrphans      bool   // eth_getBlockByHash still answers for orphaned blocks
	ForbiddenWindowLog string // substring that must never appear during the forbidden window
}

// Registry maps a client type to its ClientSpec, keyed by the short name in the service name.
var Registry = map[string]ClientSpec{
	"geth": {
		Introspects:        true,
		ReorgLogPattern:    `Chain reorg detected`,
		ServesOrphans:      true,
		ForbiddenWindowLog: `migration window`,
	},
	"erigon": {
		Introspects:   true,
		ServesOrphans: true,
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
