package main

import (
	"context"
	"encoding/json"
	"fmt"
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
// interface (proxy:, xray:, utunN, app:KEY, or "" for the default
// route) resolves to one of these so a single HTTP probe works across
// all of them.
type dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

// exitProbeURL is plaintext HTTP on purpose — it must work through a
// raw SOCKS5/HTTP-CONNECT dialer without a TLS layer of our own. The
// daemon-side cache (resolveExitIP) keeps us well under ip-api.com's
// ~45 req/min free-tier limit.
const exitProbeURL = "http://ip-api.com/json/?fields=query,countryCode,regionName,city"

// probeExitIPVia fetches the public exit IP + geo seen by ip-api.com
// when reached through dc. ok=false means the path was unreachable or
// the response was unusable (timeout, no route, rate-limited, etc.).
func probeExitIPVia(ctx context.Context, dc dialContext) (exitIP, country, region, city string, ok bool) {
	pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, exitProbeURL, nil)
	if err != nil {
		return "", "", "", "", false
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dc}}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", "", false
	}
	defer resp.Body.Close()
	var body struct {
		Query       string `json:"query"`
		CountryCode string `json:"countryCode"`
		RegionName  string `json:"regionName"`
		City        string `json:"city"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.CountryCode) != 2 {
		return "", "", "", "", false
	}
	return body.Query, body.CountryCode, body.RegionName, body.City, true
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

// dialContextForInterface builds the DialContext that egresses the way a
// rule bound to iface would route its matched traffic:
//   - ""              → system default route (what unmatched traffic uses)
//   - proxy:NAME[,..] → first existing proxy in the list
//   - xray:NAME[,..]  → first enabled xray entry's local SOCKS5 inbound
//   - app:KEY[,..]    → first running app's current utun, interface-bound
//   - utunN (literal) → that interface, interface-bound
func (d *handlerDeps) dialContextForInterface(ctx context.Context, iface string) (dialContext, error) {
	switch {
	case iface == "":
		return (&net.Dialer{}).DialContext, nil

	case proxy.IsProxyInterface(iface):
		for _, name := range proxy.ParseInterface(iface) {
			p, err := d.proxyStore.GetByName(ctx, name)
			if err != nil {
				continue
			}
			dl, err := proxy.NewDialer(p)
			if err != nil {
				continue
			}
			return proxyDialContext(dl), nil
		}
		return nil, fmt.Errorf("no usable proxy found for %q", iface)

	case xray.IsXrayInterface(iface):
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
			return proxyDialContext(dl), nil
		}
		return nil, fmt.Errorf("no enabled xray entry found for %q", iface)

	default:
		utun := iface
		if strings.HasPrefix(iface, "app:") {
			keys := parseAppKeys(iface)
			if _, resolved := d.apps.FirstAvailable(keys); resolved != "" {
				utun = resolved
			} else {
				return nil, fmt.Errorf("no running app for %q", iface)
			}
		}
		ifc, err := net.InterfaceByName(utun)
		if err != nil {
			return nil, fmt.Errorf("interface %s not found: %w", utun, err)
		}
		return boundDialContext(ifc.Index), nil
	}
}

// parseAppKeys splits "app:k1,k2" into ["k1","k2"], dropping blanks.
func parseAppKeys(iface string) []string {
	raw := strings.Split(strings.TrimPrefix(iface, "app:"), ",")
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
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
	dc, err := d.dialContextForInterface(ctx, iface)
	if err != nil {
		res.Message = err.Error()
		cacheExitPut(iface, res, exitFailTTL)
		return res
	}
	ip, country, region, city, ok := probeExitIPVia(ctx, dc)
	if !ok {
		res.Message = "couldn't reach ip-api.com through this path (timeout or unreachable)"
		cacheExitPut(iface, res, exitFailTTL)
		return res
	}
	res.ExitIP, res.Country, res.Region, res.City, res.Probed = ip, country, region, city, true
	res.Message = "ok"
	cacheExitPut(iface, res, exitOKTTL)
	return res
}
