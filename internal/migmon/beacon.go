package migmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// BeaconPeerCount reads a consensus client's connected peer count.
func BeaconPeerCount(ctx context.Context, baseURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/eth/v1/node/peer_count", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Connected string `json:"connected"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("peer_count: %w", err)
	}
	return strconv.Atoi(out.Data.Connected)
}
