package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// el is one execution client, addressed by the name ethereum-package gave it.
type el struct {
	name string
	c    *ethclient.Client
}

func dialELs(ctx context.Context, specs []string) ([]*el, error) {
	var out []*el
	for _, s := range specs {
		name, url, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--el wants name=url, got %q", s)
		}
		c, err := ethclient.DialContext(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", name, err)
		}
		out = append(out, &el{name: name, c: c})
	}
	return out, nil
}

func (e *el) head(ctx context.Context) (uint64, common.Hash, error) {
	h, err := e.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, common.Hash{}, err
	}
	return h.Number.Uint64(), h.Hash(), nil
}

// hashAt returns the canonical hash this client currently has at height n, or the zero
// hash if it does not have that height. Comparing the same height before and after is
// what makes a reorg observable: a changed hash at an unchanged height IS the reorg.
func (e *el) hashAt(ctx context.Context, n uint64) common.Hash {
	h, err := e.c.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		return common.Hash{}
	}
	return h.Hash()
}

// lowestHead is the height every client has reached, which is the only height at which
// disagreement is meaningful. Comparing at the highest head would flag a client that is
// merely one block behind.
func lowestHead(ctx context.Context, els []*el) (uint64, error) {
	var lowest uint64
	for i, e := range els {
		n, _, err := e.head(ctx)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", e.name, err)
		}
		if i == 0 || n < lowest {
			lowest = n
		}
	}
	return lowest, nil
}

// agreed reports whether every client has the same hash at height n.
func agreed(ctx context.Context, els []*el, n uint64) (bool, map[string]common.Hash) {
	seen := map[string]common.Hash{}
	var first common.Hash
	same := true
	for i, e := range els {
		h := e.hashAt(ctx, n)
		seen[e.name] = h
		if i == 0 {
			first = h
		} else if h != first {
			same = false
		}
	}
	return same, seen
}

// waitBlocks blocks until the slowest client has advanced n blocks, or ctx ends.
func waitBlocks(ctx context.Context, els []*el, n uint64) error {
	start, err := lowestHead(ctx, els)
	if err != nil {
		return err
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			cur, err := lowestHead(ctx, els)
			if err != nil {
				continue // a client mid-partition may refuse; that is expected here
			}
			if cur >= start+n {
				return nil
			}
		}
	}
}

// beacon is one consensus client's HTTP API.
type beacon struct {
	name string
	url  string
	hc   *http.Client
}

func newBeacons(specs []string) ([]*beacon, error) {
	var out []*beacon
	for _, s := range specs {
		name, url, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--cl wants name=url, got %q", s)
		}
		out = append(out, &beacon{name: name, url: url, hc: &http.Client{Timeout: 10 * time.Second}})
	}
	return out, nil
}

func (b *beacon) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+path, nil)
	if err != nil {
		return err
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// headSlot reads the current slot from the beacon head header.
func (b *beacon) headSlot(ctx context.Context) (uint64, error) {
	var r struct {
		Data struct {
			Header struct {
				Message struct {
					Slot string `json:"slot"`
				} `json:"message"`
			} `json:"header"`
		} `json:"data"`
	}
	if err := b.getJSON(ctx, "/eth/v1/beacon/headers/head", &r); err != nil {
		return 0, err
	}
	return strconv.ParseUint(r.Data.Header.Message.Slot, 10, 64)
}

type duty struct {
	Slot           uint64
	ValidatorIndex uint64
}

// duties returns the proposer assignments for an epoch, sorted by slot.
func (b *beacon) duties(ctx context.Context, epoch uint64) ([]duty, error) {
	var r struct {
		Data []struct {
			Slot           string `json:"slot"`
			ValidatorIndex string `json:"validator_index"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/eth/v1/validator/duties/proposer/%d", epoch)
	if err := b.getJSON(ctx, path, &r); err != nil {
		return nil, err
	}
	out := make([]duty, 0, len(r.Data))
	for _, d := range r.Data {
		slot, err1 := strconv.ParseUint(d.Slot, 10, 64)
		vi, err2 := strconv.ParseUint(d.ValidatorIndex, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, duty{Slot: slot, ValidatorIndex: vi})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, nil
}

// peers reports how many peers this consensus client is connected to.
//
// It is what separates "these clients disagree" from "this client is talking to nobody":
// after a heal the first is a finding and the second is the network. The beacon API returns
// the count as a STRING, not a number.
func (b *beacon) peers(ctx context.Context) (int, error) {
	var out struct {
		Data struct {
			Connected string `json:"connected"`
		} `json:"data"`
	}
	if err := b.getJSON(ctx, "/eth/v1/node/peer_count", &out); err != nil {
		return 0, err
	}
	return strconv.Atoi(out.Data.Connected)
}
