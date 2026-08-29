package migmon

import (
	"context"
	"fmt"
)

// Repeer reconnects every execution client to every other one.
//
// It exists because of how this devnet moves blocks: each consensus client
// feeds its own execution client through the engine API, so the execution
// layer's peer-to-peer mesh carries almost no traffic and ends up with one
// peer or none. That is invisible until a node misses blocks. Then its
// consensus client, far enough behind, stops replaying payloads and simply
// points the execution client at the current head - which the execution
// client has to fetch itself, from peers it does not have. The node then
// sits at its old head forever, falling further behind, looking exactly
// like a migration that failed to converge.
//
// Measured: a node stranded that way at block 147 while the chain ran on to
// 387 caught up completely within ninety seconds of being given one peer.
// So every heal ends with this, and a partition that spans the fork becomes
// survivable rather than terminal.
func Repeer(ctx context.Context, clients []Client) error {
	enodes := make([]string, len(clients))
	for i, c := range clients {
		info, err := c.NodeInfo(ctx)
		if err != nil {
			return fmt.Errorf("reading %s's node info: %w", c.Name(), err)
		}
		enodes[i] = info
	}
	var failed []string
	for i, c := range clients {
		for j, enode := range enodes {
			if i == j || enode == "" {
				continue
			}
			if err := c.AddPeer(ctx, enode); err != nil {
				failed = append(failed, fmt.Sprintf("%s -> %s: %v", c.Name(), clients[j].Name(), err))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("re-peering incomplete: %v", failed)
	}
	return nil
}
