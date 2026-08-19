// Package disruptoor speaks the native disruptoor API, which takes label selectors rather
// than the friendlier participant numbers ethereum-package accepts at start-up:
//
//	{"node-index": [1,2], "client-type": ["execution","beacon"]}
//
// A group matching no container is rejected with 500 and the whole state rolls back, so a
// disruption either applies or fails loudly. What the API cannot report is whether it had any
// EFFECT; that is what pbtchaos's own verification is for.
package disruptoor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/cli"
)

// Client is a disruptoor endpoint.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a client for the disruptoor at base.
//
// The timeout is a parameter because the callers genuinely disagree: pbtchaos is applying a
// disruption and can afford to wait, while pbtmonitor is only asking whether one is applied
// and must never let that question delay reporting a divergence.
func New(base string, timeout time.Duration) *Client {
	return &Client{base: base, hc: &http.Client{Timeout: timeout}}
}

func (d *Client) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, cli.Trim(out, 200))
	}
	return out, nil
}

// containers reports how many containers disruptoor can see. Zero means every selector
// we send will match nothing, which is the one failure mode that looks like success: the
// partition is "applied", no traffic changes, and the chain looks healthy throughout.
func (d *Client) Containers() (int, error) {
	raw, err := d.do(http.MethodGet, "/webui/api/containers", nil)
	if err != nil {
		return 0, err
	}
	var asList []json.RawMessage
	if json.Unmarshal(raw, &asList) == nil {
		return len(asList), nil
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return 0, fmt.Errorf("unrecognised container listing: %s", cli.Trim(raw, 120))
	}
	if inner, ok := asMap["containers"]; ok {
		var list []json.RawMessage
		if json.Unmarshal(inner, &list) == nil {
			return len(list), nil
		}
	}
	return len(asMap), nil
}

type group map[string][]any

func nodes(idx ...int) group {
	ids := make([]any, len(idx))
	for i, n := range idx {
		ids[i] = n
	}
	return group{"node-index": ids, "client-type": {"execution", "beacon"}}
}

// partition cuts p2p between the two groups in both directions. Engine API is untouched,
// so each side keeps driving its own execution client and the two branches grow apart.
func (d *Client) Partition(name string, a, b []int) error {
	_, err := d.do(http.MethodPut, "/v1/state", map[string]any{
		"partitions": []any{map[string]any{
			"name":      name,
			"groups":    []any{nodes(a...), nodes(b...)},
			"scope":     []string{"el_p2p", "cl_p2p"},
			"symmetric": true,
		}},
	})
	return err
}

func (d *Client) Clear() error {
	_, err := d.do(http.MethodPost, "/v1/state/clear", nil)
	return err
}

// state reports how many partitions and shaping rules are currently APPLIED. Note this
// is applied state only: it says nothing about what those rules are doing.
func (d *Client) State() (partitions, shaping int, err error) {
	raw, err := d.do(http.MethodGet, "/v1/state", nil)
	if err != nil {
		return 0, 0, err
	}
	var s struct {
		Partitions []json.RawMessage `json:"partitions"`
		Shaping    []json.RawMessage `json:"shaping"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, 0, err
	}
	return len(s.Partitions), len(s.Shaping), nil
}
