package migmon

import "strings"

// ClientSpec is the client-onboarding evidence contract: what a given
// execution client's implementation is expected to expose, so the monitor
// and the verifier can scope their checks to what that client actually
// supports instead of assuming every client behaves like the first one
// wired up. A new client onboards by adding one entry here; nothing else
// needs to change to teach the monitor and verifier its shape.
type ClientSpec struct {
	Introspects        bool   // exposes debug_migrationProgress and the shadow-root RPC
	DigestLineRequired bool   // startup log carries a binary-trie digest line
	ReorgLogPattern    string // substring a reorg notice logs
	ServesOrphans      bool   // eth_getBlockByHash still answers for orphaned blocks
	ForbiddenWindowLog string // substring that must never appear during the forbidden window
}

// Registry maps a client type to its ClientSpec. Keyed by the short name
// that appears in node/service names (e.g. "geth" in "el-1-geth-lighthouse").
var Registry = map[string]ClientSpec{
	"geth": {
		Introspects:        true,
		DigestLineRequired: true,
		ReorgLogPattern:    `Chain reorg detected`,
		ServesOrphans:      true,
		ForbiddenWindowLog: `migration window`,
	},
}

// SpecFor returns the ClientSpec whose registry key appears as a substring
// of node (e.g. "el-1-geth-lighthouse" matches "geth"), and whether one was
// found. Substring match, not exact, because node names carry the topology
// index and consensus-client pairing around the client type.
func SpecFor(node string) (ClientSpec, bool) {
	for key, spec := range Registry {
		if strings.Contains(node, key) {
			return spec, true
		}
	}
	return ClientSpec{}, false
}
