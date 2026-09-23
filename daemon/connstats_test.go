package main

import (
	"testing"
	"time"
)

func TestConnStatsWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newConnStats()
	c.now = func() time.Time { return now }

	c.established("_xray_nyc", 300*time.Millisecond)
	c.established("_xray_nyc", 800*time.Millisecond)
	c.established("_xray_la", 2*time.Second)
	c.failed(causeNoUpstream)
	c.noData("_xray_nyc")
	c.blamed("_xray_la")
	c.hedged()
	c.udpFlow(true)
	c.udpFlow(false)

	s := c.snapshot()
	if s.Conns != 4 || s.OK != 2 || s.Failed[causeNoUpstream] != 1 || s.Failed[causeNoData] != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Hedges != 1 || s.UDPFlows != 2 || s.UDPSilent != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.SetupP50 != 1000 || s.SetupP95 != 2000 {
		t.Fatalf("setup p50/p95 = %d/%d, want 1000/2000", s.SetupP50, s.SetupP95)
	}
	if len(s.Upstreams) != 2 || s.Upstreams[0].Name != "nyc" || s.Upstreams[0].Carried != 2 || s.Upstreams[0].NoData != 1 {
		t.Fatalf("upstreams = %+v", s.Upstreams)
	}
	if s.Upstreams[1].Raw != "_xray_la" || s.Upstreams[1].Blamed != 1 {
		t.Fatalf("upstreams = %+v", s.Upstreams)
	}

	// Past the window everything ages out.
	now = now.Add(connStatsWindow * time.Minute)
	if s := c.snapshot(); s.Conns != 0 || len(s.Upstreams) != 0 || s.SetupP50 != -1 {
		t.Fatalf("stale window = %+v", s)
	}
}

func TestSetupHistQuantile(t *testing.T) {
	var h setupHist
	if h.quantile(0.5) != -1 {
		t.Fatalf("empty quantile")
	}
	h.add(30 * time.Second)
	if got := h.quantile(0.5); got <= setupBinsMs[len(setupBinsMs)-1] {
		t.Fatalf("overflow bin quantile = %d", got)
	}
}

func TestNilConnStatsIsSafe(t *testing.T) {
	var c *connStats
	c.established("x", time.Second)
	c.failed(causePaused)
	c.noData("x")
	c.blamed("x")
	c.hedged()
	c.udpFlow(true)
	if s := c.snapshot(); s.Conns != 0 {
		t.Fatalf("nil snapshot = %+v", s)
	}
}
