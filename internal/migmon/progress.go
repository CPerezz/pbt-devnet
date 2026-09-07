// Package migmon holds the migration monitor's pure logic: decoding
// progress reports, per-node phase timelines, shadow-root comparison, reorg tracking.
// All I/O lives in cmd/migration-monitor.
package migmon

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexUint64 decodes a JSON number or a 0x-prefixed hex string.
type FlexUint64 uint64

func (f *FlexUint64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return fmt.Errorf("hex cursor %q: %w", s, err)
		}
		*f = FlexUint64(v)
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("cursor %q: %w", s, err)
	}
	*f = FlexUint64(v)
	return nil
}

// DirectionProgress mirrors core.DirectionProgress; field names unmarshal case-insensitively.
type DirectionProgress struct {
	Phase      string     `json:"phase"`
	Cursor     FlexUint64 `json:"cursor"`
	CursorHash string     `json:"cursorHash"`
	ShadowRoot string     `json:"shadowRoot"`
	Error      string     `json:"error"`
}

// MigrationProgress mirrors core.MigrationProgress.
type MigrationProgress struct {
	Phase  string             `json:"phase"`
	Binary *DirectionProgress `json:"binary"`
	Merkle *DirectionProgress `json:"merkle"`
}

// DecodeProgress parses one debug_migrationProgress result payload.
func DecodeProgress(raw []byte) (MigrationProgress, error) {
	var p MigrationProgress
	if err := json.Unmarshal(raw, &p); err != nil {
		return MigrationProgress{}, fmt.Errorf("migrationProgress payload: %w", err)
	}
	if p.Phase == "" {
		return MigrationProgress{}, fmt.Errorf("migrationProgress payload carries no phase: %s", raw)
	}
	return p, nil
}

// Direction phase names: idle, following, synced, parked, stalled.
const (
	DirIdle      = "idle"
	DirFollowing = "following"
	DirSynced    = "synced"
	DirParked    = "parked"
	DirStalled   = "stalled"
)

// Chain phase names.
const (
	PhaseInactive = "inactive"
	PhaseRunning  = "running"
	PhaseDone     = "done"
)

// Active reports whether a direction is live and healthy (following or synced).
func Active(d *DirectionProgress) bool {
	return d != nil && (d.Phase == DirFollowing || d.Phase == DirSynced)
}
