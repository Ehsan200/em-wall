package main

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
)

func TestStickyBindingsSetGet(t *testing.T) {
	s := newStickyBindings()
	if got := s.Get("example.com"); got != "" {
		t.Fatalf("unset key = %q, want empty", got)
	}
	s.Set("example.com", "us-la")
	if got := s.Get("example.com"); got != "us-la" {
		t.Fatalf("Get = %q, want us-la", got)
	}
}

func TestStickyBindingsExpires(t *testing.T) {
	s := newStickyBindings()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Set("example.com", "us-la")
	now = now.Add(stickyTTL + time.Second)
	if got := s.Get("example.com"); got != "" {
		t.Fatalf("stale binding = %q, want empty", got)
	}
}

// Drop is how a member that failed a destination loses it — but only if it
// is still the member that destination is on. A concurrent connection may
// already have moved it, and undoing that would send the site back to the
// upstream that just failed it.
func TestStickyBindingsDropOnlyMatchingName(t *testing.T) {
	s := newStickyBindings()
	s.Set("example.com", "us-la")
	s.Drop("example.com", "us-nj")
	if got := s.Get("example.com"); got != "us-la" {
		t.Fatalf("Get = %q, want us-la (drop named another upstream)", got)
	}
	s.Drop("example.com", "us-la")
	if got := s.Get("example.com"); got != "" {
		t.Fatalf("Get = %q, want empty after matching drop", got)
	}
}

func TestStickyBindingsNilSafe(t *testing.T) {
	var s *stickyBindings
	s.Set("example.com", "us-la")
	s.Drop("example.com", "us-la")
	if got := s.Get("example.com"); got != "" {
		t.Fatalf("nil tracker returned %q", got)
	}
}

// Entries nobody has touched are pruned so the map tracks what is actually
// being browsed rather than everything ever resolved.
func TestStickyBindingsPrunesStale(t *testing.T) {
	s := newStickyBindings()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Set("old.example.com", "us-la")
	now = now.Add(stickyTTL + stickySweepInterval)
	s.Set("new.example.com", "us-nj") // triggers the sweep
	if _, ok := s.entries["old.example.com"]; ok {
		t.Fatalf("stale entry survived the sweep")
	}
	if got := s.Get("new.example.com"); got != "us-nj" {
		t.Fatalf("Get = %q, want us-nj", got)
	}
}

// A binding moves off a member still in standing only after it has missed
// the destination at least stickyMoveMisses times over stickyMoveAfter: a
// burst of misses in one bad second is not enough, nor is one stale miss.
func TestStickyBindingsMissNeedsRunOverTime(t *testing.T) {
	s := newStickyBindings()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Set("example.com", "a")
	for i := 0; i < 5; i++ {
		if s.Miss("example.com", "a") {
			t.Fatalf("burst miss %d moved the binding", i)
		}
	}
	now = now.Add(stickyMoveAfter)
	if !s.Miss("example.com", "a") {
		t.Fatalf("misses over %s did not move the binding", stickyMoveAfter)
	}
	// A carried connection clears the run.
	s.Set("example.com", "a")
	now = now.Add(stickyMoveAfter)
	if s.Miss("example.com", "a") {
		t.Fatalf("first miss after a carry moved the binding")
	}
	// A miss against another member is ignored.
	if s.Miss("example.com", "b") {
		t.Fatalf("miss against a different member counted")
	}
}

func TestStickyBindingsMissRunResetsWhenQuiet(t *testing.T) {
	s := newStickyBindings()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Set("example.com", "a")
	s.Miss("example.com", "a")
	now = now.Add(stickyMissReset + time.Second)
	if s.Miss("example.com", "a") {
		t.Fatalf("a miss after a long quiet gap moved the binding")
	}
}

func newSettleForwarder(tr *netprobe.LatencyTracker) *proxyForwarder {
	return &proxyForwarder{
		sticky:  newStickyBindings(),
		latency: tr,
		sampler: newLogSampler(),
		logger:  log.New(io.Discard, "", 0),
	}
}

// Losing a race, or carrying through a brief outage, leaves the site on its
// member; only sustained misses or a lost seat move it.
func TestSettleBindingKeepsSiteThroughBlips(t *testing.T) {
	tr := netprobe.NewLatencyTracker(time.Hour)
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("b", 100*time.Millisecond, true)
	pf := newSettleForwarder(tr)
	now := time.Now()
	pf.sticky.now = func() time.Time { return now }
	entry := proxy.Entry{Hostname: "www.example.com", ProxyNames: []string{"a", "b"}}
	pf.settleBinding(entry, "a", []string{"a", "b"}, nil)

	// b wins a race a went first in: the site stays on a.
	pf.settleBinding(entry, "b", []string{"a", "b"}, nil)
	if got := pf.incumbent(entry); got != "a" {
		t.Fatalf("lost race moved the site to %q", got)
	}
	// a fails outright once and b carries: still a.
	pf.noteUpstreamFailure(entry, "a")
	pf.settleBinding(entry, "b", []string{"a", "b"}, []string{"a"})
	if got := pf.incumbent(entry); got != "a" {
		t.Fatalf("one failure moved the site to %q", got)
	}
	// a keeps missing for stickyMoveAfter: the site moves.
	now = now.Add(stickyMoveAfter)
	pf.settleBinding(entry, "b", []string{"a", "b"}, nil)
	if got := pf.incumbent(entry); got != "b" {
		t.Fatalf("sustained misses left the site on %q", got)
	}
}

func TestSettleBindingMovesOnLostSeatOrDeliberateRank(t *testing.T) {
	tr := netprobe.NewLatencyTracker(time.Hour)
	tr.Record("a", 100*time.Millisecond, true)
	tr.Record("b", 100*time.Millisecond, true)
	pf := newSettleForwarder(tr)
	entry := proxy.Entry{Hostname: "www.example.com", ProxyNames: []string{"a", "b"}}

	// Ranking put a healthy b ahead of a on purpose (margin / site avoid).
	pf.settleBinding(entry, "a", []string{"a", "b"}, nil)
	pf.settleBinding(entry, "b", []string{"b", "a"}, nil)
	if got := pf.incumbent(entry); got != "b" {
		t.Fatalf("deliberate rank left the site on %q", got)
	}

	// A member that left the binding loses its sites at once.
	gone := proxy.Entry{Hostname: "www.example.com", ProxyNames: []string{"a"}}
	pf.settleBinding(gone, "a", []string{"a"}, nil)
	if got := pf.incumbent(gone); got != "a" {
		t.Fatalf("member gone from binding kept the site: %q", got)
	}
}
