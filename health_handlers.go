package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

func registerHealthHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodHealthStats, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.healthStats(ctx), nil
	})
	s.Handle(ipc.MethodHealthPools, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.HealthPoolsParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
		}
		if d.xraySup == nil {
			return []ipc.PoolTimelineDTO{}, nil
		}
		window := time.Duration(p.WindowSec) * time.Second
		pools := d.xraySup.timeline.snapshot(p.Master, window, d.poolNodeNames(ctx))
		views := d.xraySup.poolStrategyViews()
		for i := range pools {
			if v, ok := views[pools[i].Master]; ok {
				pools[i].Strategy, pools[i].StrategyAuto, pools[i].StrategyReason = v.strategy, v.auto, v.reason
				if !v.since.IsZero() {
					pools[i].StrategySince = v.since.Format(time.RFC3339)
				}
			}
		}
		return pools, nil
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
		Rebinds:       snap.Rebinds,
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
			r.BreakerOpen, r.Suspect, r.FailureRate, r.RTTMs = h.Open, h.Suspect, h.FailureRate, h.RTT.Milliseconds()
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
	return poolNodeNamesFrom(ctx, d.xrayStore)
}

func poolNodeNamesFrom(ctx context.Context, xs *xray.Store) map[string]string {
	out := map[string]string{}
	if xs == nil {
		return out
	}
	subs, err := xs.ListSubs(ctx)
	if err != nil {
		return out
	}
	for _, sub := range subs {
		nodes, err := xs.ListNodes(ctx, sub.ID)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			name := n.Name
			if !strings.HasPrefix(name, sub.Name+"/") { // stored names often carry it already
				name = sub.Name + "/" + name
			}
			out[n.Fingerprint] = name
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
