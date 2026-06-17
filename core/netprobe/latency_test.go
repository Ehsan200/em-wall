package netprobe

import (
	"testing"
	"time"
)

func TestLatencyTracker_Rank(t *testing.T) {
	tr := NewLatencyTracker(time.Minute)
	tr.Record("slow", 200*time.Millisecond, true)
	tr.Record("fast", 20*time.Millisecond, true)
	tr.Record("dead", 0, false)
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
