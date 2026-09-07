package main

import (
	"errors"
	"testing"
	"time"
)

// A client at zero peers is starved only after the grace, only outside a held
// window, only once per episode, and never while its API is unreachable.
func TestStarvationDecision(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	var s starvation
	if s.observe(at(0), 0, nil, false) || s.observe(at(60), 0, nil, false) {
		t.Fatal("fired inside the grace")
	}
	if !s.observe(at(90), 0, nil, false) {
		t.Fatal("did not fire at the grace")
	}
	if s.observe(at(120), 0, nil, false) {
		t.Fatal("fired twice in one episode")
	}
	if s.observe(at(150), 2, nil, false) {
		t.Fatal("fired while peered")
	}
	// A new episode after recovery fires again.
	s.observe(at(200), 0, nil, false)
	if !s.observe(at(300), 0, nil, false) {
		t.Fatal("a fresh episode did not fire")
	}
	// Zero peers inside a held window is a partition victim, not starvation.
	var v starvation
	v.observe(at(0), 0, nil, true)
	if v.observe(at(200), 0, nil, true) {
		t.Fatal("fired inside a held window")
	}
	// Leaving the window starts the clock from zero.
	v.observe(at(210), 0, nil, false)
	if v.observe(at(280), 0, nil, false) {
		t.Fatal("counted time spent inside the window")
	}
	// An unreachable API says nothing about peers.
	var u starvation
	u.observe(at(0), 0, nil, false)
	u.observe(at(100), 0, errors.New("dial"), false)
	if u.observe(at(120), 0, nil, false) {
		t.Fatal("fired across an unreachable poll")
	}
}
