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

	old := FloorBucket(time.Now().Add(-TrafficRetention - 24*time.Hour))
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

func TestCompactTraffic_RollsUpOldRowsHourly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	hour := (time.Now().Add(-72*time.Hour).UTC().Unix() / 3600) * 3600
	mustAdd(t, s, hour, "a.com", "p1", "", 1, 10)      // hour-aligned row: the rollup target
	mustAdd(t, s, hour+60, "a.com", "p1", "g", 2, 20)  // folded into it
	mustAdd(t, s, hour+3540, "a.com", "p1", "", 4, 40) // last minute of the hour
	mustAdd(t, s, hour+120, "b.com", "p1", "", 8, 80)  // no aligned row yet: one is created
	recent := FloorBucket(time.Now())
	mustAdd(t, s, recent, "a.com", "p1", "", 16, 160) // inside TrafficRollupAge: untouched

	for i := 0; i < 2; i++ { // second pass must change nothing
		if err := s.CompactTraffic(ctx, time.Now().Add(-TrafficRollupAge)); err != nil {
			t.Fatalf("CompactTraffic: %v", err)
		}
	}

	var rows []TrafficStat
	if err := s.db.Order("bucket_ts, domain").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := []TrafficStat{
		{BucketTS: hour, Domain: "a.com", Proxy: "p1", GroupKey: "g", BytesSent: 7, BytesRecv: 70},
		{BucketTS: hour, Domain: "b.com", Proxy: "p1", BytesSent: 8, BytesRecv: 80},
		{BucketTS: recent, Domain: "a.com", Proxy: "p1", BytesSent: 16, BytesRecv: 160},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows %+v, want %d", len(rows), rows, len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func mustAdd(t *testing.T, s *Store, bucket int64, domain, proxy, group string, sent, recv int64) {
	t.Helper()
	if err := s.AddTraffic(context.Background(), bucket, domain, proxy, group, sent, recv); err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}
}

func TestAddTrafficBatch_AccumulatesInOneTx(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	bucket := FloorBucket(time.Now())

	if err := s.AddTraffic(ctx, bucket, "a.example", "p", "", 10, 20); err != nil {
		t.Fatal(err)
	}
	err := s.AddTrafficBatch(ctx, []TrafficStat{
		{BucketTS: bucket, Domain: "a.example", Proxy: "p", BytesSent: 5, BytesRecv: 5},
		{BucketTS: bucket, Domain: "b.example", Proxy: "p", BytesSent: 1, BytesRecv: 2},
		{BucketTS: bucket, Domain: "c.example", Proxy: "p"}, // zero delta: skipped
	})
	if err != nil {
		t.Fatalf("AddTrafficBatch: %v", err)
	}
	pts, err := s.QueryTraffic(ctx, bucket, bucket, TrafficByDomain, 60)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, p := range pts {
		got[p.Key] = [2]int64{p.BytesSent, p.BytesRecv}
	}
	if len(got) != 2 || got["a.example"] != [2]int64{15, 25} || got["b.example"] != [2]int64{1, 2} {
		t.Fatalf("totals = %+v", got)
	}
}
