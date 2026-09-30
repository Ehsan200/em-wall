package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
)

// Exit keys: which upstreams share a way OUT.
//
// Route keys (routekeys.go) group members by the first hop the uplink
// connects to. Members can also share the last hop: different servers,
// relays or subscriptions that all leave the internet from one IP. That
// matters in two places. A service that bans an exit IP bans every member
// behind it (observed: all five members of one set exited through a single
// address Spotify refused), and a race that hedges onto a sibling with the
// same exit gains nothing against such a ban. So the exit IP of every
// member of a multi-member binding is measured in the background and used
// as a second diversity key (diverseOrder) and as a second avoidance key
// (siteAvoid).
//
// Measuring an exit means one plain-HTTP request to ip-api.com through the
// member (probeExitIPVia). The free tier allows ~45 requests a minute, and
// the UI's own exit lookups share it, so probes are sequential, spaced by
// exitKeyProbeSpacing, and a full pass runs only every exitKeyProbeInterval.
// Exit IPs rarely move; a stale one only costs diversity, never
// correctness. A member whose exit can't be measured simply has no exit key.
const (
	exitKeyProbeInterval = 15 * time.Minute
	exitKeyProbeSpacing  = 3 * time.Second
	exitKeyFirstDelay    = 45 * time.Second // let xray and the prober settle first
)

type exitKeys struct {
	m atomic.Pointer[map[string]string] // proxy-store name → exit IP
}

// lookup returns name's exit key ("exit:" + IP), or "" when unknown.
func (e *exitKeys) lookup(name string) string {
	if e == nil {
		return ""
	}
	m := e.m.Load()
	if m == nil {
		return ""
	}
	if ip := (*m)[name]; ip != "" {
		return "exit:" + ip
	}
	return ""
}

func (e *exitKeys) snapshot() map[string]string {
	out := map[string]string{}
	if e == nil {
		return out
	}
	if m := e.m.Load(); m != nil {
		for k, v := range *m {
			out[k] = v
		}
	}
	return out
}

func (e *exitKeys) set(m map[string]string) {
	if e != nil {
		e.m.Store(&m)
	}
}

// probeExitKeys measures the exit IP of each name and publishes the result.
// A name that fails keeps its last known exit; names no longer listed are
// dropped.
func probeExitKeys(ctx context.Context, store *proxy.Store, keys *exitKeys, names []string, logger *log.Logger) {
	old := keys.snapshot()
	next := make(map[string]string, len(names))
	changed := 0
	for i, name := range names {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(exitKeyProbeSpacing):
			}
		}
		ip := old[name]
		if p, err := store.GetByName(ctx, name); err == nil {
			if d, err := proxy.NewDialer(p); err == nil {
				if got, _, _, _, ok := probeExitIPVia(ctx, proxyDialContext(d)); ok && got != "" {
					ip = got
				}
			}
		}
		if ip != "" {
			next[name] = ip
			if old[name] != ip {
				changed++
			}
		}
	}
	keys.set(next)
	if changed > 0 && logger != nil {
		// Report which members share an exit: that is the fact worth seeing.
		byIP := map[string][]string{}
		for n, ip := range next {
			byIP[ip] = append(byIP[ip], n)
		}
		shared := 0
		for _, ns := range byIP {
			if len(ns) > 1 {
				shared += len(ns)
			}
		}
		logger.Printf("em-walld: exit IPs measured for %d upstream(s): %d distinct exit(s), %d upstream(s) share one", len(next), len(byIP), shared)
	}
}
