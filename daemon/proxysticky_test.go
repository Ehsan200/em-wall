package main

import (
	"testing"
	"time"
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
