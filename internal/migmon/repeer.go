package migmon

import (
	"context"
	"fmt"
)

// Repeer reconnects every execution client to every other one. Each
// consensus client feeds its execution client through the engine API, so
// the execution p2p mesh carries little traffic and can end up with one
// peer or none, stranding a node at its old head. 90s: a node stranded at
// block 147 while the chain ran to 387 caught up fully within 90s of one peer.
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
