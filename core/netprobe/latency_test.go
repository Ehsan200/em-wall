package netprobe

import (
	"testing"
	"time"
)

func TestLatencyTracker_Rank(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("slow", 200*time.Millisecond, true)
	tr.Record("fast", 20*time.Millisecond, true)
	tr.Fail("dead") // two consecutive failures = dead
	tr.Fail("dead")
	// "unknown" never recorded.

	got := tr.Rank([]string{"slow", "dead", "unknown", "fast"})
	want := []string{"fast", "slow", "unknown", "dead"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
}

func TestLatencyTracker_RankSinglePassthrough(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	in := []string{"only"}
	got := tr.Rank(in)
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("single binding mangled: %v", got)
	}
}

func TestLatencyTracker_StaleIsUnknown(t *testing.T) {
	tr := NewLatencyTracker(time.Millisecond) // everything goes stale fast
	tr.Record("a", 10*time.Millisecond, true)
	tr.Record("b", 99*time.Millisecond, true)
	time.Sleep(5 * time.Millisecond)
	// Both stale → unknown tier → original order preserved.
	got := tr.Rank([]string{"b", "a"})
	if got[0] != "b" || got[1] != "a" {
		t.Fatalf("stale samples should preserve input order, got %v", got)
	}
}

func TestLatencyTracker_SingleFailureIsNotDead(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("a", 10*time.Millisecond, true)
	tr.Fail("b") // one strike only → unknown, not dead
	tr.Fail("c")
	tr.Fail("c") // two strikes → dead
	got := tr.Rank([]string{"c", "b", "a"})
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
}

func TestLatencyTracker_SuccessClearsStreak(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Fail("a")
	tr.Record("a", 10*time.Millisecond, true)
	tr.Fail("a") // streak restarted at 1 → still not dead
	got := tr.Rank([]string{"a", "b"})
	if got[0] != "a" {
		t.Fatalf("a should outrank never-probed b, got %v", got)
	}
}

func TestLatencyTracker_IncumbentHysteresis(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("inc", 200*time.Millisecond, true)
	tr.Record("challenger", 185*time.Millisecond, true)

	// Without an incumbent, raw latency wins.
	if got := tr.Rank([]string{"inc", "challenger"}); got[0] != "challenger" {
		t.Fatalf("plain rank = %v, want challenger first", got)
	}
	// 15ms / 7.5% is below both margins — incumbent keeps the flow.
	if got := tr.RankFrom([]string{"inc", "challenger"}, "inc"); got[0] != "inc" {
		t.Fatalf("hysteresis rank = %v, want inc first", got)
	}
}

func TestLatencyTracker_IncumbentDisplacedByRealGain(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("inc", 300*time.Millisecond, true)
	tr.Record("challenger", 100*time.Millisecond, true)
	got := tr.RankFrom([]string{"inc", "challenger"}, "inc")
	if got[0] != "challenger" {
		t.Fatalf("rank = %v, want challenger first (200ms / 66%% gain)", got)
	}
}

func TestLatencyTracker_SmallAbsoluteGainKeepsIncumbent(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("inc", 8*time.Millisecond, true)
	tr.Record("challenger", 2*time.Millisecond, true)
	// 75% better, but only 6ms — below the floor, so not worth a swap.
	if got := tr.RankFrom([]string{"inc", "challenger"}, "inc"); got[0] != "inc" {
		t.Fatalf("rank = %v, want inc first", got)
	}
}

func TestLatencyTracker_DeadIncumbentLosesSeat(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Fail("inc")
	tr.Fail("inc")
	tr.Record("other", 900*time.Millisecond, true)
	if got := tr.RankFrom([]string{"inc", "other"}, "inc"); got[0] != "other" {
		t.Fatalf("rank = %v, want other first", got)
	}
}

func TestLatencyTracker_IncumbentOrderPreservedForRest(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("a", 10*time.Millisecond, true)
	tr.Record("b", 20*time.Millisecond, true)
	tr.Record("c", 300*time.Millisecond, true)
	// c is incumbent but far worse than a → displaced; a,b keep their order.
	got := tr.RankFrom([]string{"a", "b", "c"}, "c")
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
	// b is incumbent and only 10ms behind a (below the floor) → keeps front,
	// and the others shift back without reordering among themselves.
	got = tr.RankFrom([]string{"a", "b", "c"}, "b")
	want = []string{"b", "a", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
}

// --- circuit breaker ---

// clockedTracker returns a tracker whose clock the test drives, plus the
// knob to advance it.
func clockedTracker(t *testing.T) (*LatencyTracker, func(time.Duration)) {
	t.Helper()
	tr := NewLatencyTracker(time.Minute)
	now := time.Now()
	tr.now = func() time.Time { return now }
	return tr, func(d time.Duration) { now = now.Add(d) }
}

func healthOf(t *testing.T, tr *LatencyTracker, name string) Health {
	t.Helper()
	for _, h := range tr.Snapshot() {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no health for %q", name)
	return Health{}
}

func TestBreaker_FlakyNameOpensWithoutAStreak(t *testing.T) {
	tr, advance := clockedTracker(t)
	// Alternating results: the consecutive-failure streak never reaches
	// two, so the old accounting called this name healthy forever. Half
	// its attempts fail, which is what the window sees.
	for _, ok := range []bool{false, true, false, true, false, true} {
		tr.Record("flaky", 50*time.Millisecond, ok)
		advance(30 * time.Second)
	}
	h := healthOf(t, tr, "flaky")
	if h.Fails >= deadStrikeThreshold {
		t.Fatalf("test no longer exercises the rate path: streak = %d", h.Fails)
	}
	if !h.Open {
		t.Fatalf("breaker should be open on a 50%% failure rate, got %+v", h)
	}
	tr.Record("good", 900*time.Millisecond, true)
	if got := tr.Rank([]string{"flaky", "good"}); got[0] != "good" {
		t.Fatalf("rank = %v, want the slower-but-reliable name first", got)
	}
}

func TestBreaker_NeedsMinimumEvidence(t *testing.T) {
	tr, advance := clockedTracker(t)
	// One failure in three is below the evidence floor — nothing to
	// conclude yet.
	tr.Record("a", 10*time.Millisecond, true)
	advance(time.Second)
	tr.Record("a", 0, false)
	advance(time.Second)
	tr.Record("a", 10*time.Millisecond, true)
	if healthOf(t, tr, "a").Open {
		t.Fatal("breaker opened on three samples")
	}
}

func TestBreaker_FreshSuccessDoesNotUndoDemotion(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("bad", 0, false)
	tr.Record("bad", 0, false) // streak → open
	advance(30 * time.Second)  // still inside the cooldown
	tr.Record("bad", 5*time.Millisecond, true)
	tr.Record("good", 500*time.Millisecond, true)
	if got := tr.Rank([]string{"bad", "good"}); got[0] != "good" {
		t.Fatalf("rank = %v, want good first: one 5ms probe must not clear the cooldown", got)
	}
}

func TestBreaker_RecoversAfterCooldownAndCleanWindow(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("node", 0, false)
	tr.Record("node", 0, false)
	if !healthOf(t, tr, "node").Open {
		t.Fatal("expected open breaker")
	}
	// Background probing continues while the name is demoted — that is
	// what lets it come back without anyone editing the binding.
	for i := 0; i < 9; i++ {
		advance(30 * time.Second)
		tr.Record("node", 20*time.Millisecond, true)
	}
	if h := healthOf(t, tr, "node"); h.Open {
		t.Fatalf("breaker should have closed after a clean window, got %+v", h)
	}
	if got := tr.Rank([]string{"node", "other"}); got[0] != "node" {
		t.Fatalf("recovered node should rank again, got %v", got)
	}
}

func TestBreaker_TrafficStormDoesNotOutvoteProbes(t *testing.T) {
	tr, advance := clockedTracker(t)
	// A client retry loop against one dead destination: hundreds of
	// failures through an upstream that is otherwise fine. Only one may
	// enter the window per trafficSampleInterval.
	for i := 0; i < 200; i++ {
		tr.Fail("busy")
	}
	if n := healthOf(t, tr, "busy").Samples; n != 1 {
		t.Fatalf("window absorbed %d of 200 storm failures, want 1", n)
	}
	advance(trafficSampleInterval)
	tr.Fail("busy")
	if n := healthOf(t, tr, "busy").Samples; n != 2 {
		t.Fatalf("samples = %d, want 2 after the interval elapsed", n)
	}
}

func TestBreaker_TrafficOutcomesFeedTheWindow(t *testing.T) {
	tr, advance := clockedTracker(t)
	for i := 0; i < 6; i++ {
		tr.Succeed("carrying")
		advance(trafficSampleInterval)
	}
	h := healthOf(t, tr, "carrying")
	if h.Samples != 6 || h.FailureRate != 0 {
		t.Fatalf("traffic successes not counted: %+v", h)
	}
	if h.Open {
		t.Fatal("a name carrying data must not be demoted")
	}
}

func TestBreaker_HookFiresOnBothEdges(t *testing.T) {
	tr, advance := clockedTracker(t)
	var edges []bool
	tr.OnBreakerChange(func(_ string, open bool, _ float64, _ int) { edges = append(edges, open) })
	tr.Record("n", 0, false)
	tr.Record("n", 0, false)
	for i := 0; i < 9; i++ {
		advance(30 * time.Second)
		tr.Record("n", time.Millisecond, true)
	}
	if len(edges) != 2 || !edges[0] || edges[1] {
		t.Fatalf("edges = %v, want [true false]", edges)
	}
}

func TestBreaker_WindowEntriesExpire(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("n", 0, false)
	tr.Record("n", 0, false)
	advance(breakerWindowTTL + time.Second)
	// Everything aged out; the next probe stands alone, so there is no
	// longer enough evidence to hold the breaker open.
	tr.Record("n", time.Millisecond, true)
	tr.Record("n", time.Millisecond, true)
	tr.Record("n", time.Millisecond, true)
	tr.Record("n", time.Millisecond, true)
	if h := healthOf(t, tr, "n"); h.Open {
		t.Fatalf("stale failures still holding the breaker open: %+v", h)
	}
}
