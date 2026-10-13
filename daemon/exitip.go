package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
	"golang.org/x/sys/unix"
)

// dialContext is the std net.Transport DialContext shape. Each rule
// interface (proxy:, xray:, utunN, or "" for the default route)
// resolves to one of these so a single HTTP probe works across all of
// them.
type dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

// exitProber is one public "what is my IP" service: a URL and a decoder
// for its JSON. ok=false means the reply was unusable.
type exitProber struct {
	url    string
	decode func(body []byte) (ip, country, region, city string, ok bool)
}

// exitProbers are tried in order, hedged (see probeExitIPVia). ip-api.com
// is first and plaintext HTTP — cheapest, and the daemon-side cache
// (resolveExitIP) keeps us well under its ~45 req/min free tier. The
// others are HTTPS fallbacks (http.Transport does the TLS over whatever
// dialer it's given, proxies included): some networks filter ip-api.com
// outright — observed on a mobile hotspot, where a plain-HTTP request with
// Host: ip-api.com connects and then never gets a byte back — and the
// default-route egress then read "unknown" forever.
var exitProbers = []exitProber{
	{
		url: "http://ip-api.com/json/?fields=query,countryCode,regionName,city",
		decode: func(b []byte) (string, string, string, string, bool) {
			var v struct {
				Query       string `json:"query"`
				CountryCode string `json:"countryCode"`
				RegionName  string `json:"regionName"`
				City        string `json:"city"`
			}
			if json.Unmarshal(b, &v) != nil || len(v.CountryCode) != 2 {
				return "", "", "", "", false
			}
			return v.Query, v.CountryCode, v.RegionName, v.City, true
		},
	},
	{
		url: "https://ipwho.is/?fields=ip,success,country_code,region,city",
		decode: func(b []byte) (string, string, string, string, bool) {
			var v struct {
				IP          string `json:"ip"`
				Success     bool   `json:"success"`
				CountryCode string `json:"country_code"`
				Region      string `json:"region"`
				City        string `json:"city"`
			}
			if json.Unmarshal(b, &v) != nil || !v.Success || len(v.CountryCode) != 2 {
				return "", "", "", "", false
			}
			return v.IP, v.CountryCode, v.Region, v.City, true
		},
	},
	{
		url: "https://get.geojs.io/v1/ip/geo.json",
		decode: func(b []byte) (string, string, string, string, bool) {
			var v struct {
				IP          string `json:"ip"`
				CountryCode string `json:"country_code"`
				Region      string `json:"region"`
				City        string `json:"city"`
			}
			if json.Unmarshal(b, &v) != nil || len(v.CountryCode) != 2 {
				return "", "", "", "", false
			}
			return v.IP, v.CountryCode, v.Region, v.City, true
		},
	},
}

// exitProbeHedge is how long a prober gets before the next one is started
// alongside it. A prober that fails brings the next one in at once.
const exitProbeHedge = 1500 * time.Millisecond

// probeExitIPVia fetches the public exit IP + geo seen when reached
// through dc, from the first exitProbers entry that answers. ok=false
// means every prober failed (timeout, no route, rate-limited, etc.).
func probeExitIPVia(ctx context.Context, dc dialContext) (exitIP, country, region, city string, ok bool) {
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel() // abandons probers still running once one answers
	type result struct {
		ip, country, region, city string
		ok                        bool
	}
	results := make(chan result, len(exitProbers))
	next, inFlight := 0, 0
	launch := func() {
		pr := exitProbers[next]
		next++
		inFlight++
		go func() {
			var r result
			r.ip, r.country, r.region, r.city, r.ok = probeOne(pctx, dc, pr)
			results <- r
		}()
	}
	hedge := time.NewTimer(exitProbeHedge)
	defer hedge.Stop()
	launch()
	for inFlight > 0 {
		select {
		case r := <-results:
			inFlight--
			if r.ok {
				return r.ip, r.country, r.region, r.city, true
			}
			if next < len(exitProbers) {
				launch()
				hedge.Reset(exitProbeHedge)
			}
		case <-hedge.C:
			if next < len(exitProbers) {
				launch()
				hedge.Reset(exitProbeHedge)
			}
		case <-pctx.Done():
			return "", "", "", "", false
		}
	}
	return "", "", "", "", false
}

func probeOne(ctx context.Context, dc dialContext, pr exitProber) (ip, country, region, city string, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.url, nil)
	if err != nil {
		return "", "", "", "", false
	}
	// One-shot request on a throwaway Transport: without DisableKeepAlives
	// the connection is parked in its idle pool, and a zero-value Transport
	// has no IdleConnTimeout — so every probe would hold a SOCKS stream
	// through xray (and xray's upstream connection) open indefinitely.
	// IPv4 only: ip-api.com is v4-only, so the fallbacks must not report an
	// IPv6 address for the same exit (exitKeys compares them; the status bar
	// would flip families). Proxy dialers ignore the network argument.
	dc4 := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "tcp" {
			network = "tcp4"
		}
		return dc(ctx, network, addr)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dc4, DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", "", "", "", false
	}
	return pr.decode(body)
}

// proxyDialContext adapts a proxy.Dialer (SOCKS5 / HTTP CONNECT, the
// same dialers xray entries reuse) to the DialContext shape.
func proxyDialContext(dialer proxy.Dialer) dialContext {
	return func(dctx context.Context, _, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		var hostname string
		if ip == nil {
			hostname = host
		}
		return dialer.Dial(dctx, hostname, ip, port)
	}
}

// boundDialContext pins outbound sockets to a specific interface via the
// macOS IP_BOUND_IF / IPV6_BOUND_IF socket option, so the probe egresses
// through utunN exactly the way em-wall's per-host routes would send a
// rule's matched traffic. Best-effort: a utun with no usable scoped
// route will simply time out in probeExitIPVia.
func boundDialContext(ifIndex int) dialContext {
	d := &net.Dialer{
		Control: func(network, _ string, c syscall.RawConn) error {
			var serr error
			if cerr := c.Control(func(fd uintptr) {
				if strings.HasSuffix(network, "6") {
					serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifIndex)
				} else {
					serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifIndex)
				}
			}); cerr != nil {
				return cerr
			}
			return serr
		},
	}
	return d.DialContext
}

// exitCandidate is one egress path the exit-IP probe can try. name is
// the proxy/xray entry it dials through (the literal iface for utunN,
// empty for the default route) — surfaced so a successful probe can
// report which member of a multi-binding actually answered. rankKey is
// the proxy.Store name the latency tracker keys on (== name for proxy:,
// the internal _xray_ name for xray:), used to order candidates the same
// way live traffic is ranked.
type exitCandidate struct {
	name    string
	rankKey string
	dc      dialContext
}

// rankCandidates reorders cands lowest-latency-first using the same
// LatencyTracker that proxytun.orderedNames consults at connection time,
// so the exit IP shown is the member traffic actually prefers. Unknown /
// unprobed members keep their binding order; dead ones sink last. No-op
// without a tracker or for single-candidate lists.
func (d *handlerDeps) rankCandidates(cands []exitCandidate) []exitCandidate {
	if d.latency == nil || len(cands) < 2 {
		return cands
	}
	keys := make([]string, len(cands))
	byKey := make(map[string]exitCandidate, len(cands))
	for i, c := range cands {
		keys[i] = c.rankKey
		byKey[c.rankKey] = c
	}
	out := make([]exitCandidate, 0, len(cands))
	for _, k := range d.latency.Rank(keys) {
		out = append(out, byKey[k])
	}
	return out
}

// dialCandidatesForInterface returns the ordered list of egress paths a
// rule bound to iface would try, first-available first — the SAME walk
// dialBinding (proxytun.go) does at connection time. The exit-IP probe
// then tries each in order and reports the first that answers, so a
// single down proxy/xray entry in a multi-binding rule no longer makes
// the whole exit identity read "couldn't determine" when later entries
// are healthy.
//
//   - ""              → system default route (what unmatched traffic uses)
//   - proxy:NAME[,..] → every existing proxy in the list, in order
//   - xray:NAME[,..]  → every enabled xray entry's local SOCKS5 inbound
//   - utunN (literal) → that interface, interface-bound
//
// Returns an error only when NO candidate can even be constructed
// (unknown proxy, no enabled xray, missing interface) — distinct from a
// candidate that builds but later fails to reach the probe target.
func (d *handlerDeps) dialCandidatesForInterface(ctx context.Context, iface string) ([]exitCandidate, error) {
	// A rule may be bound to an outbound set; probe what it currently
	// expands to. An unresolvable set comes back verbatim and matches no
	// case below, yielding the "unknown interface" error — which is the
	// honest answer for a rule that can't route.
	iface = d.expandIface(ctx, iface)
	switch {
	case iface == "":
		return []exitCandidate{{name: "", dc: (&net.Dialer{}).DialContext}}, nil

	case proxy.IsProxyInterface(iface):
		var out []exitCandidate
		for _, name := range proxy.ParseInterface(iface) {
			p, err := d.proxyStore.GetByName(ctx, name)
			if err != nil {
				continue
			}
			dl, err := proxy.NewDialer(p)
			if err != nil {
				continue
			}
			out = append(out, exitCandidate{name: name, rankKey: name, dc: proxyDialContext(dl)})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no usable proxy found for %q", iface)
		}
		return d.rankCandidates(out), nil

	case xray.IsXrayInterface(iface):
		var out []exitCandidate
		for _, name := range xray.ParseInterface(iface) {
			cfg, err := d.xrayStore.GetByName(ctx, name)
			if err != nil || !cfg.Enabled {
				continue
			}
			dl, err := proxy.NewDialer(proxy.Proxy{
				Protocol: proxy.ProtocolSOCKS5,
				Host:     "127.0.0.1",
				Port:     cfg.SocksPort,
			})
			if err != nil {
				continue
			}
			out = append(out, exitCandidate{
				name:    name,
				rankKey: xray.InternalProxyName(name),
				dc:      proxyDialContext(dl),
			})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no enabled xray entry found for %q", iface)
		}
		return d.rankCandidates(out), nil

	default:
		ifc, err := net.InterfaceByName(iface)
		if err != nil {
			return nil, fmt.Errorf("interface %s not found: %w", iface, err)
		}
		return []exitCandidate{{name: iface, dc: boundDialContext(ifc.Index)}}, nil
	}
}

// --- TTL cache ---------------------------------------------------------
//
// Keyed by the normalized interface string. Successes are cached long
// enough that the topbar poll and repeated per-rule clicks stay cheap
// and under ip-api.com's rate limit; failures are cached briefly so a
// user who fixes a down proxy/app sees the result without a long wait.

type exitCacheEntry struct {
	res   ipc.ExitIPResult
	until time.Time
}

var exitCache = struct {
	sync.Mutex
	m map[string]exitCacheEntry
}{m: map[string]exitCacheEntry{}}

const (
	exitOKTTL   = 3 * time.Minute
	exitFailTTL = 20 * time.Second
)

func cacheExitGet(key string) (ipc.ExitIPResult, bool) {
	exitCache.Lock()
	defer exitCache.Unlock()
	e, ok := exitCache.m[key]
	if !ok || time.Now().After(e.until) {
		return ipc.ExitIPResult{}, false
	}
	return e.res, true
}

func cacheExitPut(key string, res ipc.ExitIPResult, ttl time.Duration) {
	exitCache.Lock()
	defer exitCache.Unlock()
	exitCache.m[key] = exitCacheEntry{res: res, until: time.Now().Add(ttl)}
}

// resolveExitIP returns the egress identity for iface, served from cache
// when fresh. It never returns an error: failures come back as a
// Probed=false result with an explanatory Message so the UI can render
// them inline.
func (d *handlerDeps) resolveExitIP(ctx context.Context, iface string) ipc.ExitIPResult {
	if cached, ok := cacheExitGet(iface); ok {
		return cached
	}
	res := ipc.ExitIPResult{Interface: iface}
	cands, err := d.dialCandidatesForInterface(ctx, iface)
	if err != nil {
		res.Message = err.Error()
		cacheExitPut(iface, res, exitFailTTL)
		return res
	}

	// Probe every candidate concurrently and report the first one (in
	// binding / fallback order) that answers. Walking the whole list means
	// a single down member of a multi-binding rule (xray:A,B,C) no longer
	// masks the healthy ones — the result only reads "couldn't determine"
	// when EVERY candidate fails. Concurrency caps wall-time at one probe
	// timeout regardless of how many entries the rule binds.
	type probeResult struct {
		ip, country, region, city string
		ok                        bool
	}
	results := make([]probeResult, len(cands))
	var wg sync.WaitGroup
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, country, region, city, ok := probeExitIPVia(ctx, cands[i].dc)
			results[i] = probeResult{ip, country, region, city, ok}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if !r.ok {
			continue
		}
		res.ExitIP, res.Country, res.Region, res.City, res.Probed = r.ip, r.country, r.region, r.city, true
		res.Message = "ok"
		// For a multi-binding rule, name which member answered so the UI
		// can show the exit that traffic actually egresses through.
		if len(cands) > 1 && cands[i].name != "" {
			res.Message = "ok via " + cands[i].name
		}
		cacheExitPut(iface, res, exitOKTTL)
		return res
	}

	res.Message = "couldn't reach any public-IP service through this path (timeout or unreachable)"
	cacheExitPut(iface, res, exitFailTTL)
	return res
}
