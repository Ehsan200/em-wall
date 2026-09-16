package main

import (
	"context"
	"testing"
	"time"
)

func TestTCPHealthPausesOnlyAfterThreshold(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }
	key := tcpHealthKey("code.claude.com", 443)

	// Everything below the threshold must still be carried — a handful of
	// failed connections is ordinary, and refusing them would break a live
	// site.
	for i := 1; i < tcpStrikeThreshold; i++ {
		if d := h.strike(key); d != 0 {
			t.Fatalf("strike %d applied a penalty of %s; want none before the threshold", i, d)
		}
		if !h.admit(key) {
			t.Fatalf("refused a connection after %d strikes; threshold is %d", i, tcpStrikeThreshold)
		}
	}

	d := h.strike(key)
	if d != tcpPenaltyLadder[0] {
		t.Fatalf("threshold strike penalty = %s, want %s", d, tcpPenaltyLadder[0])
	}

	// Unrelated destinations keep flowing.
	if !h.admit(tcpHealthKey("api.anthropic.com", 443)) {
		t.Fatal("a paused destination leaked onto an unrelated one")
	}

	// Penalty expires on its own.
	now = now.Add(tcpPenaltyLadder[0] + time.Second)
	if !h.admit(key) {
		t.Fatal("still refusing after the penalty window elapsed")
	}
}

func TestTCPHealthAdmitsOneProbePerInterval(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }
	key := tcpHealthKey("code.claude.com", 443)

	for i := 0; i < tcpStrikeThreshold; i++ {
		h.strike(key)
	}

	// The storm this exists for is ~100 connections a second. Exactly one
	// of them may proceed; the rest cost nothing.
	admitted := 0
	for i := 0; i < 100; i++ {
		if h.admit(key) {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d of 100 retries within one probe interval, want 1", admitted)
	}

	// And the next interval gets a fresh probe, so recovery is noticed in
	// about a second rather than at the end of the penalty.
	now = now.Add(tcpProbeInterval)
	if !h.admit(key) {
		t.Fatal("no probe admitted in the following interval; a recovered path would stay paused")
	}
}

func TestTCPHealthSuccessClearsImmediately(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }
	key := tcpHealthKey("github.com", 443)

	for i := 0; i < tcpStrikeThreshold; i++ {
		h.strike(key)
	}
	// The first connection after a pause begins is the probe; the next one
	// inside the same interval is what gets refused.
	if !h.admit(key) {
		t.Fatal("the probe itself was refused")
	}
	if h.admit(key) {
		t.Fatal("not paused after reaching the threshold")
	}

	// One byte back from the probe is proof the path works.
	h.success(key)
	if !h.admit(key) {
		t.Fatal("still paused after a successful connection")
	}
	if d := h.strike(key); d != 0 {
		t.Fatalf("success did not reset the strike counter (penalty %s)", d)
	}
}

func TestTCPHealthProbeFailureExtendsWithoutEscalating(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }
	key := tcpHealthKey("github.com", 443)

	for i := 0; i < tcpStrikeThreshold; i++ {
		h.strike(key)
	}

	// Probes reporting back during a penalty must not ratchet the ladder —
	// otherwise a destination that stays down escalates at probe rate and
	// lands on the longest penalty within seconds.
	for i := 0; i < 20; i++ {
		now = now.Add(tcpProbeInterval)
		if !h.admit(key) {
			t.Fatal("probe not admitted during the penalty")
		}
		if d := h.strike(key); d != 0 {
			t.Fatalf("probe failure %d escalated to a new penalty of %s", i, d)
		}
	}

	// Once the destination recovers and fails again, it is one rung up —
	// not at the top.
	h.success(key)
	for i := 0; i < tcpStrikeThreshold; i++ {
		h.strike(key)
	}
	if d := h.strike(key); d != 0 && d != tcpPenaltyLadder[0] {
		t.Fatalf("penalty after recovery = %s, want the bottom rung %s", d, tcpPenaltyLadder[0])
	}
}

func TestTCPHealthPenaltyEscalates(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }
	key := tcpHealthKey("slow.example.com", 443)

	trip := func() time.Duration {
		var d time.Duration
		for i := 0; i < tcpStrikeThreshold; i++ {
			d = h.strike(key)
		}
		return d
	}
	for i, want := range tcpPenaltyLadder {
		if got := trip(); got != want {
			t.Fatalf("penalty %d = %s, want %s", i, got, want)
		}
		now = now.Add(want + time.Second)
	}
	if got := trip(); got != tcpPenaltyLadder[len(tcpPenaltyLadder)-1] {
		t.Fatalf("penalty past the ladder = %s, want it capped at %s", got, tcpPenaltyLadder[len(tcpPenaltyLadder)-1])
	}
}

func TestTCPHealthPrunesStaleEntries(t *testing.T) {
	now := time.Now()
	h := newTCPHealth()
	h.now = func() time.Time { return now }

	h.strike(tcpHealthKey("old.example.com", 443))
	now = now.Add(tcpHealthEntryTTL + time.Minute)
	h.strike(tcpHealthKey("new.example.com", 443)) // prunes on write

	if _, ok := h.entries[tcpHealthKey("old.example.com", 443)]; ok {
		t.Fatal("stale entry survived the prune")
	}
	if _, ok := h.entries[tcpHealthKey("new.example.com", 443)]; !ok {
		t.Fatal("fresh entry was pruned")
	}
}

func TestTCPHealthNilIsInert(t *testing.T) {
	var h *tcpHealth
	if !h.admit("x:443") {
		t.Fatal("nil health refused a connection")
	}
	if d := h.strike("x:443"); d != 0 {
		t.Fatalf("nil health returned penalty %s", d)
	}
	h.success("x:443") // must not panic
}

func TestDialGateReleasesSlots(t *testing.T) {
	g := newDialGate(2)
	ctx := context.Background()

	r1, ok := g.acquire(ctx)
	if !ok {
		t.Fatal("first acquire failed on an empty gate")
	}
	r2, ok := g.acquire(ctx)
	if !ok {
		t.Fatal("second acquire failed with a slot free")
	}

	// Saturated: a third caller waits rather than being refused outright,
	// and proceeds as soon as a slot frees.
	done := make(chan bool, 1)
	go func() {
		r3, ok := g.acquire(ctx)
		r3()
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("acquire succeeded past the ceiling")
	case <-time.After(20 * time.Millisecond):
	}

	r1()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("queued acquire gave up after a slot freed")
		}
	case <-time.After(time.Second):
		t.Fatal("queued acquire did not proceed after a slot freed")
	}

	// Release is idempotent: a caller that releases early and then hits a
	// deferred release must not hand back a slot twice.
	r2()
	r2()
	for i := 0; i < 2; i++ {
		if _, ok := g.acquire(ctx); !ok {
			t.Fatalf("slot %d unavailable; double release corrupted the gate", i)
		}
	}
}

func TestDialGateCancelledContextDoesNotWait(t *testing.T) {
	g := newDialGate(1)
	r, _ := g.acquire(context.Background())
	defer r()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, ok := g.acquire(ctx); ok {
		t.Fatal("acquired a slot with a cancelled context")
	}
	if d := time.Since(start); d > proxyDialGateWait/2 {
		t.Fatalf("cancelled acquire waited %s; it should return at once", d)
	}
}

func TestDialGateNilIsInert(t *testing.T) {
	var g *dialGate
	release, ok := g.acquire(context.Background())
	if !ok {
		t.Fatal("nil gate refused a dial")
	}
	release() // must not panic
}

func TestLogSamplerSuppressesRepeatsAndCounts(t *testing.T) {
	now := time.Now()
	s := newLogSampler()
	s.now = func() time.Time { return now }

	if ok, n := s.allow("k"); !ok || n != 0 {
		t.Fatalf("first occurrence: allow=%v suppressed=%d, want true/0", ok, n)
	}
	for i := 0; i < 500; i++ {
		if ok, _ := s.allow("k"); ok {
			t.Fatalf("occurrence %d logged inside the sample window", i)
		}
	}
	// A different key is independent — every distinct destination still
	// shows up the first time it is seen.
	if ok, _ := s.allow("other"); !ok {
		t.Fatal("an unrelated key was suppressed")
	}

	now = now.Add(logSampleInterval)
	ok, n := s.allow("k")
	if !ok {
		t.Fatal("nothing logged after the sample window elapsed")
	}
	if n != 500 {
		t.Fatalf("suppressed count = %d, want 500", n)
	}
	// The count resets with the line that reported it.
	now = now.Add(logSampleInterval)
	if _, n := s.allow("k"); n != 0 {
		t.Fatalf("suppressed count = %d after being reported, want 0", n)
	}
}

func TestLogSamplerPrunesStaleKeys(t *testing.T) {
	now := time.Now()
	s := newLogSampler()
	s.now = func() time.Time { return now }

	s.allow("old")
	now = now.Add(logSampleEntryTTL + time.Minute)
	s.allow("new") // prunes on write

	if _, ok := s.entries["old"]; ok {
		t.Fatal("stale key survived the prune")
	}
	if _, ok := s.entries["new"]; !ok {
		t.Fatal("fresh key was pruned")
	}
}

func TestLogSamplerNilIsInert(t *testing.T) {
	var s *logSampler
	if ok, n := s.allow("k"); !ok || n != 0 {
		t.Fatalf("nil sampler: allow=%v suppressed=%d, want true/0", ok, n)
	}
}
