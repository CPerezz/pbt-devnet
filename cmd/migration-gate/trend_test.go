package main

import (
	"context"
	"errors"
	"testing"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// headSpread is what tells a laggard catching up (spread narrowing) apart
// from a hard split (spread flat or growing).
func TestHeadSpread(t *testing.T) {
	clients := []migmon.Client{
		&fakeClient{name: "a", heads: 100},
		&fakeClient{name: "b", heads: 130},
		&fakeClient{name: "c", heads: 110},
	}
	got, err := headSpread(context.Background(), clients)
	if err != nil {
		t.Fatalf("headSpread: %v", err)
	}
	if got != 30 {
		t.Fatalf("headSpread = %d, want 30", got)
	}
}

func TestHeadSpreadSingleClientIsZero(t *testing.T) {
	clients := []migmon.Client{&fakeClient{name: "a", heads: 42}}
	got, err := headSpread(context.Background(), clients)
	if err != nil || got != 0 {
		t.Fatalf("headSpread = %d, %v, want 0, nil", got, err)
	}
}

// awaitConvergence's deadline extension hinges on this: only a strictly
// narrowing trend across all three samples counts as a genuine recovery.
func TestTrendShrinking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []uint64
		want    bool
	}{
		{"narrowing", []uint64{40, 25, 10}, true},
		{"flat", []uint64{20, 20, 20}, false},
		{"growing", []uint64{10, 20, 40}, false},
		{"one worse sample breaks the trend", []uint64{40, 10, 25}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trendShrinking(tc.samples); got != tc.want {
				t.Fatalf("trendShrinking(%v) = %v, want %v", tc.samples, got, tc.want)
			}
		})
	}
}

// healKind is the only place that decides whether a watchdog heal gets
// logged as mesh-repeer or dead-driver; verify-migration keys off the
// prefix, so the boundary condition matters more than the message text.
func TestHealKind(t *testing.T) {
	for _, tc := range []struct {
		name       string
		partsBefor int
		stateErr   error
		want       string
	}{
		{"partition was applied", 1, nil, "dead-driver: "},
		{"no partition applied", 0, nil, "mesh-repeer: "},
		{"state read failed", 1, errors.New("unreachable"), "mesh-repeer: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := healKind(tc.partsBefor, tc.stateErr); got != tc.want {
				t.Fatalf("healKind(%d, %v) = %q, want %q", tc.partsBefor, tc.stateErr, got, tc.want)
			}
		})
	}
}
