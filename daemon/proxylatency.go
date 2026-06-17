package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// Proxy latency probing. The ONLY consumer is multi-outbound rank selection
// (pick the lowest-latency working upstream when a route binds 2+ proxies),
// so we probe nothing when no such binding exists, and probe gently
// otherwise — each probe is one TLS handshake (network I/O, not CPU) and
// the ranking tolerates seconds of staleness.
const (
	proxyProbeInterval = 30 * time.Second
	proxyProbeTimeout  = 8 * time.Second
	proxyProbeParallel = 4
	// proxyLatencyTTL must exceed the interval so a sample stays valid
	// between probe rounds (and survives one skipped round).
	proxyLatencyTTL = 90 * time.Second
)

// multiBindingProxyNames returns the de-duplicated proxy.Store names that
// are bound in a route rule alongside at least one other proxy — the only
// names where latency ranking changes which upstream is used. xray names
// are translated to their hidden internal proxy names so they match store
// records. Returns nil when no multi-binding exists, letting the caller
// skip probing entirely.
func multiBindingProxyNames(rs []rules.Rule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if !r.Enabled || r.Action != rules.ActionRoute {
			continue
		}
		var names []string
		switch {
		case strings.HasPrefix(r.Interface, "proxy:"):
			names = proxy.ParseInterface(r.Interface)
		case xray.IsXrayInterface(r.Interface):
			for _, n := range xray.ParseInterface(r.Interface) {
				names = append(names, xray.InternalProxyName(n))
			}
		}
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// probeProxies probes each name (concurrency-capped) against the daemon's
// configured test endpoint and records the outcome in the tracker.
func probeProxies(ctx context.Context, store *proxy.Store, tracker *netprobe.LatencyTracker, names []string, host string, port int) {
	target := netprobe.Target{Host: host, Port: port}
	sem := make(chan struct{}, proxyProbeParallel)
	var wg sync.WaitGroup
	for _, name := range names {
		p, err := store.GetByName(ctx, name)
		if err != nil {
			tracker.Record(name, 0, false)
			continue
		}
		dialer, err := proxy.NewDialer(p)
		if err != nil {
			tracker.Record(name, 0, false)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(name string, d proxy.Dialer) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
			tracker.Probe(pctx, d, name, target)
			cancel()
		}(name, dialer)
	}
	wg.Wait()
}
