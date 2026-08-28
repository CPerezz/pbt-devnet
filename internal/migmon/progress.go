// Package migmon holds the migration monitor's pure logic: decoding the
// node's progress reports, holding per-node phase timelines to the fork
// schedule, comparing shadow roots across nodes, and walking reorgs. All
// I/O lives in cmd/migration-monitor; everything here is table-testable.
package migmon

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexUint64 decodes a JSON number or a 0x-prefixed hex string. geth
// marshals core.MigrationProgress with encoding/json defaults today, which
// makes Cursor a plain number — but hexutil wrappers are one refactor away
// on the fork, and a monitor that dies on "0x2a" would blame the wrong
// side. Tolerance here is one function; a wrong CRITICAL costs a run.
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

// DirectionProgress mirrors core.DirectionProgress (bintrie_follower.go:834-841
// at the pinned tip). Go's unmarshaler matches names case-insensitively, so
// both encoding/json defaults ("Phase") and a future tagged form ("phase")
// decode identically.
type DirectionProgress struct {
	Phase      string     `json:"phase"`
	Cursor     FlexUint64 `json:"cursor"`
	CursorHash string     `json:"cursorHash"`
	ShadowRoot string     `json:"shadowRoot"`
	Error      string     `json:"error"`
}

// MigrationProgress mirrors core.MigrationProgress (bintrie_follower.go:827-832).
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

// Direction phase names, per the follower's own comment: idle, following,
// synced, parked or stalled. The monitor treats unknown names as findings
// rather than guessing.
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

// Active reports whether a direction is live and healthy (following or
// synced). Nil directions are not active.
func Active(d *DirectionProgress) bool {
	return d != nil && (d.Phase == DirFollowing || d.Phase == DirSynced)
}
