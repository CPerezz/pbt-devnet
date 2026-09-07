package main

import (
	"testing"
)

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
