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
	// proxyProbeFastInterval is the re-check cadence for unsettled names
	// only (netprobe.LatencyTracker.Unsettled). Those are few, so this
	// costs little, and it bounds how long a node that came back stays
	// ranked behind the others.
	proxyProbeFastInterval = 10 * time.Second
	proxyProbeTimeout      = 5 * time.Second
	proxyProbeParallel     = 4
	// proxyProbeAttempts is how many URL tests each name gets per round,
	// one after another. One attempt per round could not tell a node that
	// answers every time from one that answers one time in three — it
	// looked fine whenever its single attempt landed (observed: a member
	// shown "372ms ok" that failed 2 of 3 tests by hand). Every attempt
	// enters the failure window, and the RTT is their median.
	proxyProbeAttempts = 3
	// proxyLatencyTTL must exceed the interval so a sample stays valid
	// between probe rounds (and survives one skipped round).
	proxyLatencyTTL = 90 * time.Second
)

// The ranking prober runs a URL test (netprobe.MeasureURL) — the same
// "real delay" V2Box and Hiddify show — deliberately separate from the
// proxies.test target the UI uses. The UI's default is an IP literal so a
// manual "does this proxy answer at all" check doesn't depend on proxy-side
// DNS; ranking wants the opposite — a hostname exercises the whole path an
// actual request takes (exit-side DNS, a real CDN edge, a full request and
// response), which is what the ranking is supposed to predict. It used to
// time a bare TLS handshake to www.gstatic.com, which a server can complete
// while stalling the request itself.

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

// probeProxies URL-tests each name proxyProbeAttempts times (names
// concurrency-capped, attempts sequential) against host:port and records
// every outcome in the tracker, in order.
//
// Outcomes are recorded only once the whole round is in. When EVERY probe
// in a multi-name round fails, the common cause is the local uplink, not
// the upstreams — recording it would open every breaker at once and leave
// the ranking with nothing but equally-"dead" names, then close them all
// again together (observed as every member of every set flapping in
// lockstep through a local outage). Such a round is dropped; ranking keeps
// its last good picture until the link returns.
func probeProxies(ctx context.Context, store *proxy.Store, tracker *netprobe.LatencyTracker, names []string, host string, port int) {
	sem := make(chan struct{}, proxyProbeParallel)
	var wg sync.WaitGroup
	results := make([][]netprobe.Result, len(names))
	probed := make([]bool, len(names))
	for i, name := range names {
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
		probed[i] = true
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d proxy.Dialer) {
			defer wg.Done()
			defer func() { <-sem }()
			for a := 0; a < proxyProbeAttempts && ctx.Err() == nil; a++ {
				pctx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
				results[i] = append(results[i], netprobe.MeasureURL(pctx, d, host, port, netprobe.URLTestPath))
				cancel()
			}
		}(i, dialer)
	}
	wg.Wait()

	n, ok := 0, 0
	for i := range names {
		if probed[i] {
			n++
			for _, r := range results[i] {
				if r.OK {
					ok++
				}
			}
		}
	}
	if n > 1 && ok == 0 {
		return
	}
	for i, name := range names {
		if probed[i] {
			for _, r := range results[i] {
				tracker.Record(name, r.Latency, r.OK)
			}
		}
	}
}
