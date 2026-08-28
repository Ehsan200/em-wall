package rules

import (
	"context"
	"testing"
	"time"
)

func TestFakeIPLeases_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	future := now.Add(time.Hour)

	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "ap.spotify.com", IP: "198.18.0.5", ExpiresAt: future}); err != nil {
		t.Fatalf("PutFakeIPLease: %v", err)
	}
	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "i.scdn.co", IP: "198.18.0.6", ExpiresAt: future}); err != nil {
		t.Fatalf("PutFakeIPLease: %v", err)
	}

	got, err := s.ListFakeIPLeases(ctx, now)
	if err != nil {
		t.Fatalf("ListFakeIPLeases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d leases, want 2", len(got))
	}

	// Re-put the same host with a new expiry: upsert, not a duplicate row.
	later := now.Add(2 * time.Hour)
	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "ap.spotify.com", IP: "198.18.0.5", ExpiresAt: later}); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _ = s.ListFakeIPLeases(ctx, now)
	if len(got) != 2 {
		t.Fatalf("after upsert listed %d leases, want 2", len(got))
	}

	if err := s.DeleteFakeIPLease(ctx, "i.scdn.co"); err != nil {
		t.Fatalf("DeleteFakeIPLease: %v", err)
	}
	got, _ = s.ListFakeIPLeases(ctx, now)
	if len(got) != 1 || got[0].Host != "ap.spotify.com" {
		t.Fatalf("after delete got %+v", got)
	}
}

// Two hostnames must never end up describing the same address: claiming an
// IP evicts whoever held it, or a restart would resurrect the stale binding.
func TestFakeIPLeases_ClaimEvictsPreviousHolder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	future := now.Add(time.Hour)

	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "old.example", IP: "198.18.0.9", ExpiresAt: future}); err != nil {
		t.Fatalf("put old: %v", err)
	}
	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "new.example", IP: "198.18.0.9", ExpiresAt: future}); err != nil {
		t.Fatalf("put new: %v", err)
	}

	got, err := s.ListFakeIPLeases(ctx, now)
	if err != nil {
		t.Fatalf("ListFakeIPLeases: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d leases for one IP, want 1: %+v", len(got), got)
	}
	if got[0].Host != "new.example" {
		t.Fatalf("IP held by %q, want new.example", got[0].Host)
	}
}

func TestFakeIPLeases_ExpiryFiltering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "live.example", IP: "198.18.0.2", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("put live: %v", err)
	}
	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "dead.example", IP: "198.18.0.3", ExpiresAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("put dead: %v", err)
	}

	got, err := s.ListFakeIPLeases(ctx, now)
	if err != nil {
		t.Fatalf("ListFakeIPLeases: %v", err)
	}
	if len(got) != 1 || got[0].Host != "live.example" {
		t.Fatalf("expired lease was listed: %+v", got)
	}

	if err := s.DeleteExpiredFakeIPLeases(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredFakeIPLeases: %v", err)
	}
	var n int64
	if err := s.db.Model(&FakeIPLease{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after sweep %d rows remain, want 1", n)
	}
}

func TestPutFakeIPLease_RejectsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.PutFakeIPLease(ctx, FakeIPLease{IP: "198.18.0.4", ExpiresAt: time.Now()}); err == nil {
		t.Fatal("expected error on empty host")
	}
	if err := s.PutFakeIPLease(ctx, FakeIPLease{Host: "x.example", ExpiresAt: time.Now()}); err == nil {
		t.Fatal("expected error on empty ip")
	}
}
