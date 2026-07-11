package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

// Subscription fetch tuning.
const (
	subDirectTimeout = 30 * time.Second // per-attempt cap for the direct fetch
	subViaTimeout    = 25 * time.Second // per-attempt cap for a via-outbound fetch
	subErrorRetry    = 5 * time.Minute  // retry a failed subscription sooner than its interval
	subSchedTick     = 60 * time.Second // how often the scheduler re-evaluates due subscriptions
	subMaxFallback   = 8                // cap fallback outbounds tried per fetch
)

// subFetcher fetches subscription URLs and replaces each subscription's
// node pool. It deliberately does NOT reconcile xray: at this layer node
// rows are just data. Materializing active nodes into the running xray
// process (and thus into master dialer balancers) is a separate, live
// gRPC step — so a routine refresh never restarts xray or drops flows.
type subFetcher struct {
	xrayStore  *xray.Store
	proxyStore *proxy.Store
	logger     *log.Logger
	// onChange, when set, is invoked after a successful refresh so the
	// supervisor can live-sync any dialer slots that draw on the changed
	// subscription (no xray restart). Errors are logged, not fatal.
	onChange func(context.Context) error
}

func newSubFetcher(xs *xray.Store, ps *proxy.Store, logger *log.Logger) *subFetcher {
	return &subFetcher{xrayStore: xs, proxyStore: ps, logger: logger}
}

// Refresh fetches one subscription and swaps its node pool. LastFetched
// and LastError are recorded regardless of outcome. Returns the number of
// nodes stored on success.
func (f *subFetcher) Refresh(ctx context.Context, sub xray.Subscription) (int, error) {
	body, via, err := f.fetchBody(ctx, sub)
	if err != nil {
		_ = f.xrayStore.SetSubFetchResult(ctx, sub.ID, err.Error())
		return 0, err
	}
	nodes, err := xray.ParseSubscriptionBody(body, sub.Name)
	if err != nil {
		_ = f.xrayStore.SetSubFetchResult(ctx, sub.ID, err.Error())
		return 0, err
	}
	if err := f.xrayStore.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		_ = f.xrayStore.SetSubFetchResult(ctx, sub.ID, err.Error())
		return 0, err
	}
	_ = f.xrayStore.SetSubFetchResult(ctx, sub.ID, "")
	f.logger.Printf("subscription %q: %d nodes%s", sub.Name, len(nodes), via)
	if f.onChange != nil {
		if err := f.onChange(ctx); err != nil {
			f.logger.Printf("subscription %q: live dialer sync failed: %v", sub.Name, err)
		}
	}
	return len(nodes), nil
}

// fetchBody tries a direct HTTP GET first, then — since a subscription
// host is often censored — falls back through existing enabled outbounds
// (user proxies, then hidden _xray_ inbounds). The first that returns a
// body wins. Returns a " (via NAME)" suffix for logging when a fallback
// was used.
func (f *subFetcher) fetchBody(ctx context.Context, sub xray.Subscription) ([]byte, string, error) {
	dctx, cancel := context.WithTimeout(ctx, subDirectTimeout)
	body, err := xray.FetchSubscriptionBody(dctx, directHTTPClient(subDirectTimeout), sub.URL, sub.UserAgent)
	cancel()
	if err == nil {
		return body, "", nil
	}
	firstErr := err

	for i, p := range f.fallbackProxies(ctx) {
		if i >= subMaxFallback {
			break
		}
		cli, cerr := proxyHTTPClient(p, subViaTimeout)
		if cerr != nil {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, subViaTimeout)
		body, err := xray.FetchSubscriptionBody(vctx, cli, sub.URL, sub.UserAgent)
		cancel()
		if err == nil {
			return body, " (via " + p.Name + ")", nil
		}
	}
	return nil, "", fmt.Errorf("direct fetch failed (%v); no fallback outbound succeeded", firstErr)
}

// fallbackProxies orders candidate via-outbounds: user proxies first,
// then hidden _xray_ rows. A disabled xray entry's row is harmless —
// nothing listens on its port, so the dial just fails and we move on.
func (f *subFetcher) fallbackProxies(ctx context.Context) []proxy.Proxy {
	all, err := f.proxyStore.List(ctx)
	if err != nil {
		return nil
	}
	var user, hidden []proxy.Proxy
	for _, p := range all {
		if xray.IsInternalProxyName(p.Name) {
			hidden = append(hidden, p)
		} else {
			user = append(user, p)
		}
	}
	return append(user, hidden...)
}

// runSubScheduler refreshes due subscriptions immediately, then on a
// ticker until ctx is cancelled.
func (f *subFetcher) runSubScheduler(ctx context.Context) {
	f.refreshDue(ctx)
	t := time.NewTicker(subSchedTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.refreshDue(ctx)
		}
	}
}

// refreshOne fetches a single subscription by ID (manual refresh path).
func (f *subFetcher) refreshOne(ctx context.Context, id int64) (int, error) {
	sub, err := f.xrayStore.GetSub(ctx, id)
	if err != nil {
		return 0, err
	}
	return f.Refresh(ctx, sub)
}

// RefreshAll force-fetches every enabled subscription regardless of its
// due time (manual "refresh all").
func (f *subFetcher) RefreshAll(ctx context.Context) {
	subs, err := f.xrayStore.ListSubs(ctx)
	if err != nil {
		return
	}
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		if _, err := f.Refresh(ctx, sub); err != nil {
			f.logger.Printf("subscription %q: refresh failed: %v", sub.Name, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (f *subFetcher) refreshDue(ctx context.Context) {
	subs, err := f.xrayStore.ListSubs(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	for _, sub := range subs {
		if !subDue(sub, now) {
			continue
		}
		if _, err := f.Refresh(ctx, sub); err != nil {
			f.logger.Printf("subscription %q: refresh failed: %v", sub.Name, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// subDue reports whether a subscription should be fetched now: never
// fetched, past its interval, or (after a prior error) past the shorter
// retry window. Disabled subscriptions are never due.
func subDue(sub xray.Subscription, now time.Time) bool {
	if !sub.Enabled {
		return false
	}
	if sub.LastFetched.IsZero() {
		return true
	}
	if sub.LastError != "" {
		return now.Sub(sub.LastFetched) >= subErrorRetry
	}
	return now.Sub(sub.LastFetched) >= sub.EffectiveInterval()
}

// directHTTPClient builds a client that ignores any environment proxy so
// the "direct" attempt is genuinely direct.
func directHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			// One-shot fetch: don't pool idle connections (each Refresh can
			// build several of these; keep-alive would leave sockets lingering
			// until GC).
			DisableKeepAlives: true,
		},
	}
}

// proxyHTTPClient adapts a proxy.Dialer into an http.Client transport so
// a subscription can be fetched through an upstream proxy or a hidden
// _xray_ SOCKS inbound.
func proxyHTTPClient(p proxy.Proxy, timeout time.Duration) (*http.Client, error) {
	d, err := proxy.NewDialer(p)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, err
			}
			var ip net.IP
			if pip := net.ParseIP(host); pip != nil {
				ip, host = pip, ""
			}
			return d.Dial(ctx, host, ip, port)
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true, // one-shot via-fetch; don't pool idle conns
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}
