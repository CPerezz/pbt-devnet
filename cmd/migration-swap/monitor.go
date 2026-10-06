package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

var httpc = &http.Client{Timeout: 10 * time.Second}

// postLifecycle logs a node's new step and tells the monitor. Best-effort: the monitor's
// view is a convenience, not a dependency the swap has to wait on.
func (r *runner) postLifecycle(node int, step string, anchor uint64, hash, detail string) {
	say("node-%d %s (anchor %d) %s", node, step, anchor, detail)
	body, err := json.Marshal(migmon.Lifecycle{Node: node, Step: step, Anchor: anchor, Hash: hash, Detail: detail})
	if err != nil {
		say("lifecycle node=%d step=%s: encoding: %v", node, step, err)
		return
	}
	resp, err := httpc.Post(r.monitorURL+"/api/lifecycle", "application/json", bytes.NewReader(body))
	if err != nil {
		say("lifecycle node=%d step=%s: %v", node, step, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		say("lifecycle node=%d step=%s: http %d", node, step, resp.StatusCode)
	}
}

// beaconHealthy reports whether a consensus client's own health endpoint is green.
func beaconHealthy(baseURL string) bool {
	resp, err := httpc.Get(baseURL + "/eth/v1/node/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
}
