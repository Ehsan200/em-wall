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
