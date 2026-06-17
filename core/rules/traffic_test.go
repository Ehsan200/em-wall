package rules

import (
	"context"
	"testing"
	"time"
)

func TestFloorBucket(t *testing.T) {
	// 1-minute storage width: any time within a minute floors to its start.
	base := time.Date(2026, 6, 17, 10, 7, 30, 0, time.UTC) // 10:07:30
	want := time.Date(2026, 6, 17, 10, 7, 0, 0, time.UTC).Unix()
	if got := FloorBucket(base); got != want {
		t.Fatalf("FloorBucket = %d, want %d", got, want)
	}
	// Exact boundary stays put.
	b := time.Date(2026, 6, 17, 10, 7, 0, 0, time.UTC)
	if got := FloorBucket(b); got != want {
		t.Fatalf("FloorBucket(boundary) = %d, want %d", got, want)
	}
}

func TestAddTraffic_Accumulates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	bucket := FloorBucket(time.Now())

	if err := s.AddTraffic(ctx, bucket, "api.anthropic.com", "proxyA", "anthropic", 100, 200); err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}
	// Same key again → bytes add, not overwrite.
	if err := s.AddTraffic(ctx, bucket, "api.anthropic.com", "proxyA", "anthropic", 50, 25); err != nil {
		t.Fatalf("AddTraffic 2: %v", err)
	}
	// Zero delta is a no-op (no error, no new row).
	if err := s.AddTraffic(ctx, bucket, "api.anthropic.com", "proxyA", "anthropic", 0, 0); err != nil {
		t.Fatalf("AddTraffic zero: %v", err)
	}

	pts, err := s.QueryTraffic(ctx, bucket, bucket, TrafficByDomain, 60)
	if err != nil {
		t.Fatalf("QueryTraffic: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d points, want 1: %+v", len(pts), pts)
	}
	if pts[0].BytesSent != 150 || pts[0].BytesRecv != 225 {
		t.Fatalf("totals = sent %d recv %d, want 150/225", pts[0].BytesSent, pts[0].BytesRecv)
	}
	if pts[0].Key != "api.anthropic.com" {
		t.Fatalf("key = %q, want domain", pts[0].Key)
	}
}

func TestQueryTraffic_GroupDimension(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	bucket := FloorBucket(time.Now())

	// Two domains in the same group + one proxy → group-sum collapses them.
	mustAdd(t, s, bucket, "api.anthropic.com", "p1", "anthropic", 100, 0)
	mustAdd(t, s, bucket, "claude.ai", "p1", "anthropic", 200, 0)
	// An ungrouped domain reports under "ungrouped".
	mustAdd(t, s, bucket, "example.com", "p1", "", 10, 5)

	pts, err := s.QueryTraffic(ctx, bucket, bucket, TrafficByGroup, 60)
	if err != nil {
		t.Fatalf("QueryTraffic: %v", err)
	}
	got := map[string]int64{}
	for _, p := range pts {
		got[p.Key] += p.BytesSent
	}
	if got["anthropic"] != 300 {
		t.Fatalf("anthropic sent = %d, want 300", got["anthropic"])
	}
	if got["ungrouped"] != 10 {
		t.Fatalf("ungrouped sent = %d, want 10", got["ungrouped"])
	}
}

func TestSweepTraffic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	old := FloorBucket(time.Now().Add(-8 * 24 * time.Hour))
	recent := FloorBucket(time.Now())
	mustAdd(t, s, old, "old.com", "p1", "", 1, 1)
	mustAdd(t, s, recent, "new.com", "p1", "", 1, 1)

	if err := s.SweepTraffic(ctx, time.Now().Add(-TrafficRetention)); err != nil {
		t.Fatalf("SweepTraffic: %v", err)
	}
	pts, err := s.QueryTraffic(ctx, 0, recent, TrafficByDomain, 60)
	if err != nil {
		t.Fatalf("QueryTraffic: %v", err)
	}
	if len(pts) != 1 || pts[0].Key != "new.com" {
		t.Fatalf("after sweep got %+v, want only new.com", pts)
	}
}

func TestQueryTraffic_RebucketsCoarser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Two adjacent 1-minute buckets inside the same 5-minute window.
	base := (time.Now().UTC().Unix() / 300) * 300 // a 5-min boundary
	mustAdd(t, s, base, "a.com", "p1", "", 100, 0)
	mustAdd(t, s, base+60, "a.com", "p1", "", 200, 0)

	// At 1-min resolution: two separate points.
	fine, err := s.QueryTraffic(ctx, base, base+300, TrafficByDomain, 60)
	if err != nil {
		t.Fatalf("QueryTraffic 60: %v", err)
	}
	if len(fine) != 2 {
		t.Fatalf("1-min got %d points, want 2", len(fine))
	}

	// At 5-min resolution: collapsed into one bucket summing both.
	coarse, err := s.QueryTraffic(ctx, base, base+300, TrafficByDomain, 300)
	if err != nil {
		t.Fatalf("QueryTraffic 300: %v", err)
	}
	if len(coarse) != 1 {
		t.Fatalf("5-min got %d points, want 1", len(coarse))
	}
	if coarse[0].BucketTS != base || coarse[0].BytesSent != 300 {
		t.Fatalf("5-min bucket = ts %d sent %d, want %d/300", coarse[0].BucketTS, coarse[0].BytesSent, base)
	}
}

func mustAdd(t *testing.T, s *Store, bucket int64, domain, proxy, group string, sent, recv int64) {
	t.Helper()
	if err := s.AddTraffic(context.Background(), bucket, domain, proxy, group, sent, recv); err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}
}
