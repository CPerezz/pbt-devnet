package main

import "testing"

func TestParticipantForUnevenStakeUsesPrefixSum(t *testing.T) {
	c := &chaos{cfg: config{validatorCounts: []uint64{128, 256, 128, 128}}}
	cases := map[uint64]int{
		0: 1, 127: 1,
		128: 2, 383: 2,
		384: 3, 511: 3,
		512: 4, 639: 4,
	}
	for idx, want := range cases {
		if got := c.participantFor(idx); got != want {
			t.Errorf("participantFor(%d) = %d, want %d", idx, got, want)
		}
	}
}

// TestParticipantForUniformFallbackMatchesFormula is the regression evidence that leaving
// --validator-counts unset reproduces today's uniform mapping exactly, across the full
// validator range a 4-node, 128-per-node devnet actually uses.
func TestParticipantForUniformFallbackMatchesFormula(t *testing.T) {
	c := &chaos{cfg: config{validatorsPer: 128, nodeCount: 4}}
	for i := uint64(0); i < 4*128; i++ {
		want := int(i/128) + 1
		if got := c.participantFor(i); got != want {
			t.Fatalf("participantFor(%d) = %d, want %d (uniform formula)", i, got, want)
		}
	}
}

func TestParseValidatorCountsRejectsNonPositiveAndNonNumeric(t *testing.T) {
	for _, bad := range []string{"128,0,128", "128,-1,128", "128,abc,128", "128,,128"} {
		if _, err := parseValidatorCounts(bad); err == nil {
			t.Errorf("parseValidatorCounts(%q) succeeded, want an error", bad)
		}
	}
}

func TestClampDepthUnboundedByDefault(t *testing.T) {
	for _, requested := range []uint64{0, 1, 10, 1_000_000} {
		applied, clamped := clampDepth(requested, 0)
		if applied != requested || clamped {
			t.Errorf("clampDepth(%d, 0) = (%d, %v), want (%d, false)", requested, applied, clamped, requested)
		}
	}
}

func TestClampDepthClampsAboveMax(t *testing.T) {
	applied, clamped := clampDepth(10, 3)
	if applied != 3 || !clamped {
		t.Fatalf("clampDepth(10, 3) = (%d, %v), want (3, true)", applied, clamped)
	}
	applied, clamped = clampDepth(2, 3)
	if applied != 2 || clamped {
		t.Fatalf("clampDepth(2, 3) = (%d, %v), want (2, false) -- under the max needs no clamp", applied, clamped)
	}
}

func TestResolveDepthDefaultsAndClampsHTTPOverride(t *testing.T) {
	// No override: falls back to the default depth, untouched by an unset --max-depth.
	applied, requested, clamped, err := resolveDepth("", 10, 0)
	if err != nil || applied != 10 || requested != 10 || clamped {
		t.Fatalf("resolveDepth(\"\", 10, 0) = (%d, %d, %v, %v), want (10, 10, false, nil)", applied, requested, clamped, err)
	}

	// Override present, no max-depth: passes through exactly, matching today's behaviour.
	applied, requested, clamped, err = resolveDepth("50", 10, 0)
	if err != nil || applied != 50 || requested != 50 || clamped {
		t.Fatalf("resolveDepth(\"50\", 10, 0) = (%d, %d, %v, %v), want (50, 50, false, nil)", applied, requested, clamped, err)
	}

	// Override present and above --max-depth: clamped, with both values reported.
	applied, requested, clamped, err = resolveDepth("50", 10, 5)
	if err != nil || applied != 5 || requested != 50 || !clamped {
		t.Fatalf("resolveDepth(\"50\", 10, 5) = (%d, %d, %v, %v), want (5, 50, true, nil)", applied, requested, clamped, err)
	}

	// A bad query value is still a bad request, unaffected by --max-depth.
	if _, _, _, err := resolveDepth("not-a-number", 10, 5); err == nil {
		t.Fatal("resolveDepth(\"not-a-number\", ...) succeeded, want an error")
	}
}

// TestIsolationWindowUnboundedByDefault is the regression evidence that the periodic
// isolation cadence is untouched when --max-depth is left at its default of 0: the window
// this command has always used (two slots, one duty) stays exactly two slots, and the depth
// it reports stays exactly 1, whether or not --isolate-for was customised.
func TestIsolationWindowUnboundedByDefault(t *testing.T) {
	c := &chaos{cfg: config{slotSeconds: 12_000_000_000, isolateFor: 24_000_000_000}} // 12s slots, 2-slot window
	window, depth, requested, clamped := c.isolationWindow()
	if window != c.cfg.isolateFor || depth != 1 || requested != 1 || clamped {
		t.Fatalf("isolationWindow() = (%v, %d, %d, %v), want (%v, 1, 1, false)",
			window, depth, requested, clamped, c.cfg.isolateFor)
	}
}

func TestIsolationWindowClampsToMaxDepth(t *testing.T) {
	// A 10-slot window (depth 9) asked for, but --max-depth caps it to depth 3, i.e. 4 slots.
	c := &chaos{cfg: config{slotSeconds: 12_000_000_000, isolateFor: 10 * 12_000_000_000, maxDepth: 3}}
	window, depth, requested, clamped := c.isolationWindow()
	if !clamped || depth != 3 || requested != 9 {
		t.Fatalf("isolationWindow() = (%v, %d, %d, %v), want depth 3, requested 9, clamped true",
			window, depth, requested, clamped)
	}
	if want := 4 * c.cfg.slotSeconds; window != want {
		t.Fatalf("isolationWindow() window = %v, want %v (4 slots)", window, want)
	}
}

// Protected-node coverage: a pinned scenario minority naming a protected node must fall
// back to the rotation rather than proceeding, and the rotation itself must never return a
// protected node.
func TestMinorityForFallsBackWhenPickIsProtected(t *testing.T) {
	c := &chaos{els: make([]*el, 4), cfg: config{protected: map[int]bool{2: true}}}
	if got := c.minorityFor(2); got == 2 {
		t.Fatalf("minorityFor(2) = %d, want a fallback away from the protected node", got)
	}
}

func TestMinorityForKeepsAnUnprotectedPick(t *testing.T) {
	c := &chaos{els: make([]*el, 4), cfg: config{protected: map[int]bool{2: true}}}
	if got := c.minorityFor(3); got != 3 {
		t.Fatalf("minorityFor(3) = %d, want 3 (not protected, in range)", got)
	}
}

func TestMinorityForFallsBackWhenPickIsOutOfRange(t *testing.T) {
	c := &chaos{els: make([]*el, 4), cfg: config{protected: map[int]bool{}}}
	if got := c.minorityFor(0); got < 1 || got > 4 {
		t.Fatalf("minorityFor(0) = %d, want a valid fallback in 1..4", got)
	}
	if got := c.minorityFor(9); got < 1 || got > 4 {
		t.Fatalf("minorityFor(9) = %d, want a valid fallback in 1..4", got)
	}
}
