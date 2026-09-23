package main

import (
	"context"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
)

// An upstream that carried nothing lately but is demoted by the breaker
// must still show up — that is exactly the one worth seeing.
func TestHealthStatsJoinsBreakerView(t *testing.T) {
	lat := netprobe.NewLatencyTracker(time.Minute)
	lat.Record("_xray_nyc", 300*time.Millisecond, true)
	for i := 0; i < 6; i++ {
		lat.Record("_xray_dead", 0, false)
	}
	cs := newConnStats()
	cs.established("_xray_nyc", 400*time.Millisecond)
	d := &handlerDeps{latency: lat, connHealth: cs}

	h := d.healthStats(context.Background())
	byName := map[string]int{}
	for i, u := range h.Upstreams {
		byName[u.Name] = i
	}
	nyc, ok := byName["nyc"]
	if !ok || h.Upstreams[nyc].Connections != 1 || h.Upstreams[nyc].RTTMs != 300 {
		t.Fatalf("nyc row = %+v", h.Upstreams)
	}
	dead, ok := byName["dead"]
	if !ok || !h.Upstreams[dead].BreakerOpen || h.Upstreams[dead].Connections != 0 {
		t.Fatalf("dead row = %+v", h.Upstreams)
	}
	if h.Connections != 1 || h.Succeeded != 1 || h.ParkedNodes == nil {
		t.Fatalf("totals = %+v", h)
	}
}
