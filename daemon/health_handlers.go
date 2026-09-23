package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
)

func registerHealthHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodHealthStats, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.healthStats(ctx), nil
	})
}

func (d *handlerDeps) healthStats(ctx context.Context) ipc.HealthStatsDTO {
	snap := d.connHealth.snapshot()
	out := ipc.HealthStatsDTO{
		WindowSec:     connStatsWindow * 60,
		Connections:   snap.Conns,
		Succeeded:     snap.OK,
		Failed:        snap.Failed,
		SetupP50Ms:    snap.SetupP50,
		SetupP95Ms:    snap.SetupP95,
		ExtraAttempts: snap.Hedges,
		UDPFlows:      snap.UDPFlows,
		UDPSilent:     snap.UDPSilent,
		Upstreams:     []ipc.UpstreamHealthDTO{},
		ParkedNodes:   []ipc.ParkedNodeDTO{},
	}

	// Join connection outcomes with the breaker/probe view, so an upstream
	// that is ranked last but carried nothing recently still shows up.
	rows := map[string]*ipc.UpstreamHealthDTO{}
	var order []string
	row := func(raw string) *ipc.UpstreamHealthDTO {
		if r, ok := rows[raw]; ok {
			return r
		}
		r := &ipc.UpstreamHealthDTO{Name: displayUpstream(raw), SetupP50Ms: -1}
		rows[raw] = r
		order = append(order, raw)
		return r
	}
	for _, u := range snap.Upstreams {
		r := row(u.Raw)
		r.Connections, r.Blamed, r.NoData, r.SetupP50Ms = u.Carried, u.Blamed, u.NoData, u.SetupP50
	}
	if d.latency != nil {
		for _, h := range d.latency.Snapshot() {
			r := row(h.Name)
			r.BreakerOpen, r.FailureRate, r.RTTMs = h.Open, h.FailureRate, h.RTT.Milliseconds()
		}
	}
	for _, raw := range order {
		out.Upstreams = append(out.Upstreams, *rows[raw])
	}

	if d.xraySup != nil {
		out.XrayRestarts, out.XrayLiveApplies = d.xraySup.Counters()
		parked := d.xraySup.parker.list()
		names := d.poolNodeNames(ctx)
		for _, p := range parked {
			name := names[p.Key]
			if name == "" {
				name = memberDisplayName(p.Key)
			}
			out.ParkedNodes = append(out.ParkedNodes, ipc.ParkedNodeDTO{Name: name, Until: p.Until.Format(time.RFC3339)})
		}
	}
	return out
}

// poolNodeNames maps subscription node fingerprints (their DialerMember
// keys) to "sub/node" display names. Best-effort: store errors just leave
// keys unnamed.
func (d *handlerDeps) poolNodeNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	if d.xrayStore == nil {
		return out
	}
	subs, err := d.xrayStore.ListSubs(ctx)
	if err != nil {
		return out
	}
	for _, sub := range subs {
		nodes, err := d.xrayStore.ListNodes(ctx, sub.ID)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			out[n.Fingerprint] = sub.Name + "/" + n.Name
		}
	}
	return out
}

// memberDisplayName renders a non-subscription member key ("xray-NAME",
// "proxy-NAME") for display.
func memberDisplayName(key string) string {
	for _, p := range []string{"xray-", "proxy-"} {
		if strings.HasPrefix(key, p) {
			return strings.TrimPrefix(key, p)
		}
	}
	return key
}
