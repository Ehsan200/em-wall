package netprobe

import (
	"reflect"
	"testing"
	"time"
)

func TestHandshakeTimingReranks(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tr := NewLatencyTracker(time.Hour)
	tr.now = func() time.Time { return now }
	// Probes say a is a bit faster.
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("b", 120*time.Millisecond, true)
	if got := tr.Rank([]string{"b", "a"}); got[0] != "a" {
		t.Fatalf("probe ranking = %v, want a first", got)
	}
	// Real traffic says otherwise: on the same destinations a keeps taking
	// three times as long as b.
	for i := 0; i < 6; i++ {
		for _, d := range []string{"site:x.com", "site:y.com"} {
			tr.ObserveHandshake("b", d, 200*time.Millisecond)
			tr.ObserveHandshake("a", d, 600*time.Millisecond)
		}
	}
	if got := tr.Rank([]string{"a", "b"}); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("with traffic timing = %v, want [b a]", got)
	}
	// The evidence ages out.
	now = now.Add(handshakeTTL + time.Minute)
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("b", 120*time.Millisecond, true)
	if got := tr.Rank([]string{"b", "a"}); got[0] != "a" {
		t.Fatalf("after TTL = %v, want probe ranking back", got)
	}
}

// A destination only one name ever carried has no norm to compare with:
// a far-away site must not make its one carrier look slow.
func TestHandshakeNeedsMixedDestination(t *testing.T) {
	tr := NewLatencyTracker(time.Hour)
	tr.Record("a", 100*time.Millisecond, true)
	for i := 0; i < 10; i++ {
		tr.ObserveHandshake("a", "site:far.example", 2*time.Second)
	}
	s := tr.samples["a"]
	if f := s.handshakeFactor(tr.now()); f != 1 {
		t.Fatalf("factor = %v from a destination only a carried, want 1", f)
	}
	// Unknown names are not created by traffic timing.
	tr.ObserveHandshake("ghost", "site:far.example", time.Second)
	if _, has := tr.samples["ghost"]; has {
		t.Fatal("ObserveHandshake created a sample for a never-probed name")
	}
}

func TestHandshakeFactorBounds(t *testing.T) {
	now := time.Now()
	s := sample{slow: 100, slowN: handshakeMinSamples, slowAt: now}
	if f := s.handshakeFactor(now); f > 3.01 {
		t.Fatalf("unclamped factor %v", f)
	}
}
