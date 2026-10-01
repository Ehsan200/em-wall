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

// openFlaky drives name into an open breaker through the rate path: half
// its probes fail, never two in a row.
func openFlaky(t *testing.T, tr *LatencyTracker, advance func(time.Duration), name string) {
	t.Helper()
	isOpen := func() bool {
		tr.mu.RLock()
		defer tr.mu.RUnlock()
		return tr.samples[name].open
	}
	for i := 0; i < 2*breakerWindow && !isOpen(); i++ {
		tr.Record(name, 50*time.Millisecond, i%2 == 1)
		advance(10 * time.Second)
	}
	if !healthOf(t, tr, name).Open {
		t.Fatalf("%s: breaker did not open", name)
	}
}

func TestBreaker_FreshSuccessDoesNotUndoDemotion(t *testing.T) {
	tr, advance := clockedTracker(t)
	openFlaky(t, tr, advance, "bad") // still inside the cooldown
	tr.Record("bad", 5*time.Millisecond, true)
	tr.Record("good", 500*time.Millisecond, true)
	if got := tr.Rank([]string{"bad", "good"}); got[0] != "good" {
		t.Fatalf("rank = %v, want good first: one 5ms probe must not clear the cooldown", got)
	}
}

func TestBreaker_RecoversAfterCooldownAndCleanWindow(t *testing.T) {
	tr, advance := clockedTracker(t)
	openFlaky(t, tr, advance, "node")
	// Background probing continues while the name is demoted — that is
	// what lets it come back without anyone editing the binding.
	for i := 0; i < 10; i++ {
		advance(10 * time.Second)
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
	openFlaky(t, tr, advance, "n")
	for i := 0; i < 10; i++ {
		advance(10 * time.Second)
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

func TestLatencyTrackerReset(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	for i := 0; i < 6; i++ {
		tr.Record("dead", 0, false)
	}
	tr.Record("fast", 10*time.Millisecond, true)
	tr.Reset()
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("after Reset: %+v", snap)
	}
	if got := tr.Rank([]string{"dead", "fast"}); got[0] != "dead" {
		t.Fatalf("after Reset both are unknown and keep binding order, got %v", got)
	}
}

// --- outages (suspect) ---

func TestOutage_DeadNodeReturnsOnFirstSuccess(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("good", 400*time.Millisecond, true)
	// Down for a couple of minutes: many failures, all in a row.
	for i := 0; i < 12; i++ {
		tr.Record("node", 0, false)
		advance(10 * time.Second)
	}
	h := healthOf(t, tr, "node")
	if !h.Suspect || h.Open {
		t.Fatalf("a node that is simply down is suspect, not breaker-open: %+v", h)
	}
	if got := tr.Rank([]string{"node", "good"}); got[0] != "good" {
		t.Fatalf("rank = %v, want good first while node is down", got)
	}
	// It starts working: one probe brings it straight back.
	tr.Record("node", 100*time.Millisecond, true)
	tr.Record("good", 400*time.Millisecond, true)
	h = healthOf(t, tr, "node")
	if h.Suspect || h.Open || h.FailureRate != 0 {
		t.Fatalf("recovered node still held down: %+v", h)
	}
	// Back in rotation at once — ahead of a member that is down — but
	// behind a steady one until its probation is over.
	tr.Record("other", 0, false)
	tr.Record("other", 0, false)
	if got := tr.Rank([]string{"other", "good", "node"}); got[0] != "good" || got[1] != "node" {
		t.Fatalf("rank = %v, want [good node other] while node is on probation", got)
	}
	advance(probationWindow)
	tr.Record("node", 100*time.Millisecond, true)
	tr.Record("good", 400*time.Millisecond, true)
	if got := tr.Rank([]string{"good", "node"}); got[0] != "node" {
		t.Fatalf("rank = %v, want the recovered faster node first after probation", got)
	}
	// Its outage no longer counts against it: one later hiccup is noise.
	advance(10 * time.Second)
	tr.Record("node", 0, false)
	if h := healthOf(t, tr, "node"); h.Open {
		t.Fatalf("one failure after an outage opened the breaker: %+v", h)
	}
}

func TestOutage_TrafficSuccessEndsOutage(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Fail("n")
	tr.Fail("n")
	if !healthOf(t, tr, "n").Suspect {
		t.Fatal("expected suspect after two failures")
	}
	advance(time.Second) // inside trafficSampleInterval: success skips the window
	tr.Succeed("n")
	h := healthOf(t, tr, "n")
	if h.Suspect || h.Samples != 0 {
		t.Fatalf("outage failures should be gone after a carried connection: %+v", h)
	}
}

func TestOutage_FlappingOpensBreaker(t *testing.T) {
	tr, advance := clockedTracker(t)
	for i := 0; i < flapOutages; i++ {
		tr.Record("n", 0, false)
		tr.Record("n", 0, false)
		tr.Record("n", 30*time.Millisecond, true)
		advance(time.Minute)
	}
	if h := healthOf(t, tr, "n"); !h.Open {
		t.Fatalf("%d outages in %s should open the breaker: %+v", flapOutages, flapWindow, h)
	}
}

func TestOutage_SpreadOutOutagesDoNotFlap(t *testing.T) {
	tr, advance := clockedTracker(t)
	for i := 0; i < flapOutages; i++ {
		tr.Record("n", 0, false)
		tr.Record("n", 0, false)
		tr.Record("n", 30*time.Millisecond, true)
		advance(flapWindow/2 + time.Second)
	}
	if h := healthOf(t, tr, "n"); h.Open {
		t.Fatalf("outages spread past the flap window opened the breaker: %+v", h)
	}
}

func TestBreaker_CooldownEscalatesAndDecays(t *testing.T) {
	tr, advance := clockedTracker(t)
	state := func() sample {
		tr.mu.RLock()
		defer tr.mu.RUnlock()
		return tr.samples["n"]
	}
	closeIt := func() {
		t.Helper()
		for i := 0; i < 60 && state().open; i++ {
			advance(10 * time.Second)
			tr.Record("n", 20*time.Millisecond, true)
		}
		if state().open {
			t.Fatal("breaker never closed")
		}
	}
	want := breakerCooldown
	for trip := 1; trip <= 3; trip++ {
		openFlaky(t, tr, advance, "n")
		if s := state(); s.trips != trip || s.cooldown() != want {
			t.Fatalf("trip %d: trips=%d cooldown=%s, want %d / %s", trip, s.trips, s.cooldown(), trip, want)
		}
		// Still open just before the cooldown ends, however clean the record.
		advance(want - 11*time.Second)
		tr.Record("n", 20*time.Millisecond, true)
		if !state().open {
			t.Fatalf("trip %d: closed before its %s cooldown", trip, want)
		}
		closeIt()
		want *= 2
	}
	// A long quiet spell forgets the history.
	advance(breakerTripMemory + time.Minute)
	openFlaky(t, tr, advance, "n")
	if s := state(); s.trips != 1 || s.cooldown() != breakerCooldown {
		t.Fatalf("after a quiet spell: trips=%d cooldown=%s, want 1 / %s", s.trips, s.cooldown(), breakerCooldown)
	}
}

func TestBreaker_CooldownCapped(t *testing.T) {
	if got := (sample{trips: 50}).cooldown(); got != breakerCooldownMax {
		t.Fatalf("cooldown = %s, want cap %s", got, breakerCooldownMax)
	}
}

func TestReset_GraceDropsFailures(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Reset()
	tr.Record("n", 0, false)
	tr.Record("n", 0, false)
	tr.Record("ok", 10*time.Millisecond, true)
	if got := len(tr.Snapshot()); got != 1 {
		t.Fatalf("failures inside the reset grace were recorded: %+v", tr.Snapshot())
	}
	advance(resetGrace + time.Second)
	tr.Record("n", 0, false)
	tr.Record("n", 0, false)
	if !healthOf(t, tr, "n").Suspect {
		t.Fatal("failures after the grace must count")
	}
}

func TestRank_SuspectBehindUnknownLeastBadFirst(t *testing.T) {
	tr, advance := clockedTracker(t)
	// "mostly" had a good record before going down; "never" never answered.
	for i := 0; i < 4; i++ {
		tr.Record("mostly", 300*time.Millisecond, true)
		advance(time.Second)
	}
	tr.Record("mostly", 0, false)
	tr.Record("mostly", 0, false)
	tr.Record("never", 0, false)
	tr.Record("never", 0, false)
	tr.Record("fast-suspect", 50*time.Millisecond, true)
	advance(time.Second)
	for i := 0; i < 4; i++ {
		tr.Record("fast-suspect", 0, false)
	}
	got := tr.Rank([]string{"never", "fast-suspect", "unknown", "mostly"})
	want := []string{"unknown", "mostly", "fast-suspect", "never"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
}

func TestUnsettled(t *testing.T) {
	tr, _ := clockedTracker(t)
	tr.Record("ok", 10*time.Millisecond, true)
	tr.Record("once", 0, false)
	tr.Fail("down")
	tr.Fail("down")
	got := tr.Unsettled([]string{"ok", "once", "down", "new"})
	want := []string{"once", "down", "new"}
	if len(got) != len(want) {
		t.Fatalf("unsettled = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unsettled = %v, want %v", got, want)
		}
	}
}

// --- stability ---

func TestRank_StableBeatsSlightlyFasterFlaky(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("steady", 300*time.Millisecond, true)
	tr.Record("jumpy", 0, false) // dropped a connection a moment ago…
	tr.Record("jumpy", 200*time.Millisecond, true)
	if got := tr.Rank([]string{"jumpy", "steady"}); got[0] != "steady" {
		t.Fatalf("rank = %v, want steady first: 100ms is not worth a fresh failure", got)
	}
	// …and once that failure has faded, raw speed decides again.
	advance(recentFailWindow + time.Second)
	tr.Record("steady", 300*time.Millisecond, true)
	tr.Record("jumpy", 200*time.Millisecond, true)
	if got := tr.Rank([]string{"steady", "jumpy"}); got[0] != "jumpy" {
		t.Fatalf("rank = %v, want jumpy first after its failure faded", got)
	}
}

func TestRank_MuchFasterStillWinsDespiteFailure(t *testing.T) {
	tr, _ := clockedTracker(t)
	tr.Record("steady", 900*time.Millisecond, true)
	tr.Record("fast", 0, false)
	tr.Record("fast", 150*time.Millisecond, true)
	if got := tr.Rank([]string{"steady", "fast"}); got[0] != "fast" {
		t.Fatalf("rank = %v, want fast first: 6x faster outweighs one failure", got)
	}
}

func TestRank_FailureRateWeighsCost(t *testing.T) {
	tr, advance := clockedTracker(t)
	// Same RTT and the same most recent failure, so the recent-failure
	// penalty is equal; lossy failed twice as often over the window.
	for i := 0; i < 10; i++ {
		tr.Record("clean", 250*time.Millisecond, i != 5)
		tr.Record("lossy", 250*time.Millisecond, i != 1 && i != 3 && i != 5)
		advance(10 * time.Second)
	}
	if got := tr.Rank([]string{"lossy", "clean"}); got[0] != "clean" {
		t.Fatalf("rank = %v, want clean first", got)
	}
}

// --- uplink-wide failures ---

// On a lossy uplink every name fails a third of the time together. That is
// the uplink; no single member deserves the breaker for it.
func TestBreaker_LossyUplinkDemotesNobody(t *testing.T) {
	tr, advance := clockedTracker(t)
	names := []string{"a", "b", "c", "d"}
	for i := 0; i < breakerWindow; i++ {
		for _, n := range names {
			tr.Record(n, 300*time.Millisecond, i%3 != 0)
		}
		advance(10 * time.Second)
	}
	for _, n := range names {
		if h := healthOf(t, tr, n); h.Open {
			t.Fatalf("%s demoted for an uplink-wide failure rate: %+v", n, h)
		}
	}
}

// One name failing far more than its peers is still demoted.
func TestBreaker_WorseThanPeersStillOpens(t *testing.T) {
	tr, advance := clockedTracker(t)
	for i := 0; i < breakerWindow; i++ {
		tr.Record("a", 300*time.Millisecond, true)
		tr.Record("b", 300*time.Millisecond, true)
		tr.Record("c", 300*time.Millisecond, i%10 != 0)
		tr.Record("bad", 300*time.Millisecond, i%2 == 1)
		advance(10 * time.Second)
	}
	if h := healthOf(t, tr, "bad"); !h.Open {
		t.Fatalf("a member failing half its attempts among clean peers must open: %+v", h)
	}
}

// Outages that hit most names at once are the uplink and never add up to
// flapping.
func TestOutage_LockstepOutagesDoNotFlap(t *testing.T) {
	tr, advance := clockedTracker(t)
	names := []string{"a", "b", "c"}
	for round := 0; round < flapOutages+1; round++ {
		for _, n := range names {
			tr.Record(n, 0, false)
			tr.Record(n, 0, false)
		}
		for _, n := range names {
			tr.Record(n, 300*time.Millisecond, true)
		}
		advance(time.Minute)
	}
	for _, n := range names {
		if h := healthOf(t, tr, n); h.Open {
			t.Fatalf("%s demoted for outages the whole uplink shared: %+v", n, h)
		}
	}
}

// One failure (probe below the outage streak) keeps the incumbent first:
// the race hedges past it if it really is silent, and moving on one blip
// sent sites to a new exit on every hiccup.
func TestLatencyTracker_UnknownIncumbentKeepsSeat(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("inc", 100*time.Millisecond, true)
	tr.Record("other", 100*time.Millisecond, true)
	tr.Record("inc", 0, false)
	if got := tr.RankFrom([]string{"other", "inc"}, "inc"); got[0] != "inc" {
		t.Fatalf("rank = %v, want inc held first after one failure", got)
	}
}

// A failed connection extends the streak but doesn't knock the name out of
// the healthy tier — only a failed probe (a measurement) does.
func TestLatencyTracker_TrafficFailureKeepsHealthy(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("b", time.Second, true)
	tr.Fail("a")
	if got := tr.Rank([]string{"b", "a"}); got[0] != "a" {
		t.Fatalf("rank = %v, want a still healthy and first", got)
	}
	tr.Fail("a") // second in a row: an outage
	if got := tr.Rank([]string{"a", "b"}); got[0] != "b" {
		t.Fatalf("rank = %v, want suspect a behind b", got)
	}
}

func TestLatencyTracker_SeatGrace(t *testing.T) {
	tr := NewLatencyTracker(time.Hour)
	now := time.Now()
	tr.now = func() time.Time { return now }
	if got := tr.Seat("never-seen"); got != SeatHeld {
		t.Fatalf("unknown name seat = %v, want held", got)
	}
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("a", 0, false)
	if got := tr.Seat("a"); got != SeatHeld {
		t.Fatalf("one failure seat = %v, want held", got)
	}
	tr.Record("a", 0, false)
	if got := tr.Seat("a"); got != SeatOutage {
		t.Fatalf("fresh outage seat = %v, want outage", got)
	}
	now = now.Add(SeatGrace)
	if got := tr.Seat("a"); got != SeatLost {
		t.Fatalf("outage past grace seat = %v, want lost", got)
	}
	tr.Record("a", 100*time.Millisecond, true)
	if got := tr.Seat("a"); got != SeatHeld {
		t.Fatalf("recovered seat = %v, want held", got)
	}
}

// A member that just came back from an outage must not take a site off a
// steady incumbent on one low probe — that is what moved sites every
// 30-60s between flapping members.
func TestProbation_RecoveredMemberDoesNotDisplaceSteadyIncumbent(t *testing.T) {
	tr, advance := clockedTracker(t)
	tr.Record("steady", 800*time.Millisecond, true)
	tr.Record("flappy", 0, false)
	tr.Record("flappy", 0, false)
	advance(5 * time.Second)
	tr.Record("flappy", 100*time.Millisecond, true)
	if got := tr.RankFrom([]string{"flappy", "steady"}, "steady"); got[0] != "steady" {
		t.Fatalf("rank = %v, want steady incumbent kept over a member on probation", got)
	}
	// And a steady member takes a site off one on probation.
	if got := tr.RankFrom([]string{"flappy", "steady"}, "flappy"); got[0] != "steady" {
		t.Fatalf("rank = %v, want steady ahead of an incumbent on probation", got)
	}
}

// One noisy probe doesn't swing the ranking: it is on the median.
func TestMedianRTT_OneSpikeDoesNotReorder(t *testing.T) {
	tr, _ := clockedTracker(t)
	for i := 0; i < 4; i++ {
		tr.Record("a", 300*time.Millisecond, true)
		tr.Record("b", 500*time.Millisecond, true)
	}
	tr.Record("a", 3*time.Second, true) // one spike
	if got := tr.Rank([]string{"b", "a"}); got[0] != "a" {
		t.Fatalf("rank = %v, want a first despite one slow probe", got)
	}
}
