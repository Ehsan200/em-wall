package main

import (
	"testing"
	"time"
)

func TestUDPHealthBlocksAfterConsecutiveStrikes(t *testing.T) {
	now := time.Now()
	h := newUDPHealth()
	h.now = func() time.Time { return now }
	key := udpHealthKey("docs.google.com", 443)

	if d := h.strike(key); d != 0 {
		t.Fatalf("first strike applied a penalty of %s; want none", d)
	}
	if h.blocked(key) {
		t.Fatal("blocked after a single strike; one dead association is not a dead path")
	}

	d := h.strike(key)
	if d != udpPenaltyLadder[0] {
		t.Fatalf("second strike penalty = %s, want %s", d, udpPenaltyLadder[0])
	}
	if !h.blocked(key) {
		t.Fatal("not blocked after reaching the strike threshold")
	}

	// Other destinations are unaffected.
	if h.blocked(udpHealthKey("play.google.com", 443)) {
		t.Fatal("strike leaked to an unrelated destination")
	}

	// Penalty expires on its own.
	now = now.Add(udpPenaltyLadder[0] + time.Second)
	if h.blocked(key) {
		t.Fatal("still blocked after the penalty window elapsed")
	}
}

func TestUDPHealthSuccessClearsStrikes(t *testing.T) {
	h := newUDPHealth()
	key := udpHealthKey("ssl.gstatic.com", 443)

	h.strike(key)
	h.success(key)
	if d := h.strike(key); d != 0 {
		t.Fatalf("a working flow between strikes did not reset the counter (penalty %s)", d)
	}
}

func TestUDPHealthPenaltyEscalatesAndResets(t *testing.T) {
	now := time.Now()
	h := newUDPHealth()
	h.now = func() time.Time { return now }
	key := udpHealthKey("beacons.gvt2.com", 443)

	for i, want := range udpPenaltyLadder {
		h.strike(key)
		got := h.strike(key)
		if got != want {
			t.Fatalf("penalty %d = %s, want %s", i, got, want)
		}
		now = now.Add(want + time.Second)
	}
	// Ladder caps rather than growing without bound.
	h.strike(key)
	if got := h.strike(key); got != udpPenaltyLadder[len(udpPenaltyLadder)-1] {
		t.Fatalf("penalty past the ladder = %s, want it capped at %s", got, udpPenaltyLadder[len(udpPenaltyLadder)-1])
	}

	// A success drops the destination back to the bottom of the ladder.
	h.success(key)
	h.strike(key)
	if got := h.strike(key); got != udpPenaltyLadder[0] {
		t.Fatalf("penalty after recovery = %s, want %s", got, udpPenaltyLadder[0])
	}
}

func TestUDPHealthPrunesStaleEntries(t *testing.T) {
	now := time.Now()
	h := newUDPHealth()
	h.now = func() time.Time { return now }

	h.strike(udpHealthKey("old.example.com", 443))
	now = now.Add(udpHealthEntryTTL + time.Minute)
	h.strike(udpHealthKey("new.example.com", 443)) // prunes on write

	if _, ok := h.entries[udpHealthKey("old.example.com", 443)]; ok {
		t.Fatal("stale entry survived the prune")
	}
	if _, ok := h.entries[udpHealthKey("new.example.com", 443)]; !ok {
		t.Fatal("fresh entry was pruned")
	}
}

func TestUDPHealthNilIsInert(t *testing.T) {
	var h *udpHealth
	if h.blocked("x:443") {
		t.Fatal("nil health blocked a flow")
	}
	if d := h.strike("x:443"); d != 0 {
		t.Fatalf("nil health returned penalty %s", d)
	}
	h.success("x:443") // must not panic
}
