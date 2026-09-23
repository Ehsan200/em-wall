package main

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/ehsan/em-wall/core/xray"
)

// Route keys: which upstreams share a way in.
//
// A set's members are not independent just because they are separate
// entries. On the live install the five nyc members were three routes:
// two dialed the same server directly, two went through the same relay
// host, and two masters tunnelled through the same subscription pool. When
// one stalls because of its route, a sibling on that route stalls too, so
// the race (raceRound) spends its second attempt on a member with a
// different route whenever one exists.
//
// The key is the first thing the user's uplink actually connects to:
//   - a master entry: its pool — "pool:" + the sorted dialer refs, the same
//     grouping that shares one dialer slot;
//   - any other xray entry or proxy: "host:" + its server host (the port is
//     ignored — different ports on one relay machine fail together);
//   - unknown: the name itself, i.e. assumed independent.

type routeKeys struct {
	m atomic.Pointer[map[string]string] // proxy-store name → key
}

// set publishes keys for xray entries (keyed by their hidden proxy name).
func (r *routeKeys) set(m map[string]string) {
	if r != nil {
		r.m.Store(&m)
	}
}

func (r *routeKeys) lookup(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	m := r.m.Load()
	if m == nil {
		return "", false
	}
	k, ok := (*m)[name]
	return k, ok
}

// xrayRouteKeys computes route keys for every xray entry.
func xrayRouteKeys(entries []xray.Config) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		name := xray.InternalProxyName(e.Name)
		if refs, err := xray.ParseDialer(e.Dialer); err == nil && len(refs) > 0 {
			out[name] = "pool:" + dialerGroupKey(refs)
			continue
		}
		if h := xray.OutboundServerHost(e.Outbound); h != "" {
			out[name] = "host:" + h
		}
	}
	return out
}

// routeOf returns name's route key: published xray keys first, then a plain
// proxy's host, else the name itself.
func (pf *proxyForwarder) routeOf(name string) string {
	if k, ok := pf.routes.lookup(name); ok {
		return k
	}
	if pf.store != nil && !strings.HasPrefix(name, xray.InternalProxyName("")) {
		// Served from the store's in-memory cache.
		if p, err := pf.store.GetByName(context.Background(), name); err == nil && p.Host != "" {
			return "host:" + strings.ToLower(p.Host)
		}
	}
	return "name:" + name
}
