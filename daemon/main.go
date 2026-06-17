// em-walld is the privileged firewall daemon. It runs as root (via
// LaunchDaemon), owns the SQLite rule store, runs the DNS proxy on
// 127.0.0.1:53, manages per-host routes, and exposes an IPC socket
// for the Wails UI to drive it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ehsan/em-wall/core/applocator"
	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/pfctl"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/routing"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// lsofProvider adapts core/routing's exported LsofUtunOwners to the
// LsofProvider interface that applocator depends on. Keeps applocator
// free of any direct dependency on routing.
type lsofProvider struct{}

func (lsofProvider) LsofUtunOwners() map[string]string { return routing.LsofUtunOwners() }

// publicFallbackDNS is the last-resort upstream used only when no
// system/DHCP/VPN resolver can be discovered AND validated. Without a
// fallback, em-wall would refuse to activate and leave the host with no
// DNS, so we fall back to well-known always-on public resolvers
// (Cloudflare + Google). Overridable at launch via -upstream.
var publicFallbackDNS = []string{"1.1.1.1:53", "8.8.8.8:53"}

func main() {
	var (
		dbPath         = flag.String("db", "/usr/local/var/em-wall/rules.db", "path to SQLite database")
		sockPath       = flag.String("socket", ipc.DefaultSocketPath, "path to IPC unix socket")
		listenAddr     = flag.String("listen", "127.0.0.1:53", "DNS proxy listen address")
		upstream       = flag.String("upstream", strings.Join(publicFallbackDNS, ","), "comma-separated upstream DNS servers")
		noAutoActivate = flag.Bool("no-auto-activate", false, "do not touch system DNS on startup (for tests / dev)")
		proxyTestTgt   = flag.String("proxy-test-target", defaultProxyTestTarget, "host:port dialed through a proxy by proxies.test to check reachability")
		xrayBinary     = flag.String("xray-binary", "/usr/local/bin/em-wall-xray", "path to the xray-core binary the supervisor runs")
		xrayDataDir    = flag.String("xray-data-dir", "/usr/local/share/em-wall", "directory containing xray's geoip.dat + geosite.dat (XRAY_LOCATION_ASSET)")
		xrayRuntimeDir = flag.String("xray-runtime-dir", "/usr/local/var/em-wall/xray", "directory where the supervisor writes the generated xray config")
		xrayLogDir     = flag.String("xray-log-dir", "/usr/local/var/log", "directory where xray writes its access/error logs; empty disables xray logging")
	)
	flag.Parse()

	// Parse the proxy reachability probe target once. An invalid value
	// falls back to the default rather than failing startup — the test
	// feature is non-critical and shouldn't keep the daemon from
	// booting.
	proxyTestHost, proxyTestPort := parseProxyTestTarget(*proxyTestTgt)

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("em-walld: mkdir db dir: %v", err)
	}

	store, err := rules.Open(*dbPath)
	if err != nil {
		log.Fatalf("em-walld: open store: %v", err)
	}
	defer store.Close()

	proxyStore, err := proxy.Open(*dbPath)
	if err != nil {
		log.Fatalf("em-walld: open proxy store: %v", err)
	}
	defer proxyStore.Close()

	xrayStore, err := xray.Open(*dbPath)
	if err != nil {
		log.Fatalf("em-walld: open xray store: %v", err)
	}
	defer xrayStore.Close()

	// Supervisor probes the binary; in dev (binary not installed) it
	// constructs disabled and Reconcile is a no-op for the subprocess
	// but still keeps hidden proxy rows in sync.
	xraySup := newXraySupervisor(*xrayBinary, *xrayDataDir, *xrayRuntimeDir, *xrayLogDir,
		xrayStore, proxyStore, log.Default())
	if err := xraySup.Reconcile(context.Background()); err != nil {
		log.Printf("em-walld: initial xray reconcile failed (continuing): %v", err)
	}
	defer xraySup.Stop()

	// IP→proxy mapping populated by the DNS layer, consumed by the
	// netstack TCP handler. minTTL keeps a mapping alive long enough
	// that a connection opened right after resolution isn't orphaned
	// by an aggressive sweep.
	proxyTable := proxy.NewTable(60 * time.Second)
	router := routing.New(nil)
	proxyLatency := netprobe.NewLatencyTracker(proxyLatencyTTL)
	proxyTunnel, proxyTunName := startProxyTunnel(proxyStore, proxyTable, router, proxyLatency, log.Default())
	if proxyTunnel != nil {
		defer proxyTunnel.Stop()
	}

	engine := decision.New(store)
	if err := engine.Reload(context.Background()); err != nil {
		log.Fatalf("em-walld: load rules: %v", err)
	}

	pf := pfctl.New(nil)
	sysDNS := NewSystemDNS(nil)
	apps := applocator.NewResolver(lsofProvider{})
	apps.Refresh() // populate initial app→utun mapping

	logSink := &storeLogSink{store: store}

	// Pick upstream: stored setting > flag default. The setting is
	// populated when the user clicks "Activate" in the UI, capturing
	// whatever the system was using before we hijacked it.
	upstreams := loadUpstream(store, *upstream)
	fwd := dnsproxy.NewMultiUpstream(upstreams, 3*time.Second)

	dnsServer, err := dnsproxy.New(dnsproxy.Config{
		Listen:       *listenAddr,
		Decider:      engine,
		Forwarder:    fwd,
		Routes:       router,
		Interfaces:   dnsproxy.DefaultInterfaceChecker,
		Apps:         apps,
		Proxies:      &proxyResolver{store: proxyStore, table: proxyTable},
		EnableFakeIP: true,
		ProxyTun:     proxyTunName,
		Logs:         logSink,
		Logger:       log.Default(),
	})
	if err != nil {
		log.Fatalf("em-walld: dnsproxy: %v", err)
	}

	ipcSrv := ipc.NewServer(*sockPath, log.Default())
	deps := &handlerDeps{
		store:         store,
		proxyStore:    proxyStore,
		proxyTable:    proxyTable,
		xrayStore:     xrayStore,
		xraySup:       xraySup,
		xrayDataDir:   *xrayDataDir,
		engine:        engine,
		router:        router,
		pf:            pf,
		sysDNS:        sysDNS,
		dnsServer:     dnsServer,
		apps:          apps,
		listenAddr:    *listenAddr,
		upstream:      joinCSV(upstreams),
		startedAt:     time.Now(),
		proxyTestHost: proxyTestHost,
		proxyTestPort: proxyTestPort,
	}
	registerHandlers(ipcSrv, deps)

	// Restore pf state from settings. Default-on so fresh installs
	// block DoH/DoT without the user having to toggle it.
	if v, _ := store.GetSetting(context.Background(), "block_encrypted_dns", "true"); v == "true" {
		if err := pf.Enable(context.Background()); err != nil {
			log.Printf("em-walld: pf enable failed (continuing): %v", err)
		}
	}

	// Auto-activate the DNS hijack unless explicitly disabled (via flag
	// or persisted setting). Default-on so the daemon is useful out of
	// the box; activation now validates upstream and refuses to brick
	// DNS if nothing works.
	if *noAutoActivate {
		log.Printf("em-walld: -no-auto-activate set, leaving system DNS alone")
	} else if v, _ := store.GetSetting(context.Background(), "system_dns_active", "true"); v == "true" {
		if err := deps.activateSystemDNS(context.Background()); err != nil {
			log.Printf("em-walld: auto-activate failed (continuing): %v", err)
		} else {
			log.Printf("em-walld: system DNS hijack active")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(6)

	go func() {
		defer wg.Done()
		log.Printf("em-walld: dns proxy listening on %s", *listenAddr)
		if err := dnsServer.Start(ctx); err != nil {
			log.Printf("em-walld: dns proxy stopped: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		log.Printf("em-walld: ipc socket at %s", *sockPath)
		if err := ipcSrv.Serve(ctx); err != nil {
			log.Printf("em-walld: ipc stopped: %v", err)
		}
	}()

	// Periodic route TTL sweeper. Also sweeps the proxy IP→proxy table
	// on the same tick so expired mappings don't outlive their routes.
	go func() {
		defer wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				router.SweepExpired(ctx)
				proxyTable.SweepExpired()
			}
		}
	}()

	// Proxy latency prober: ranks multi-outbound route bindings by measured
	// latency. Probes ONLY proxies bound alongside another (nothing to rank
	// otherwise), so a single-outbound setup does no network work here.
	go func() {
		defer wg.Done()
		t := time.NewTicker(proxyProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rs, err := store.List(ctx)
				if err != nil {
					continue
				}
				names := multiBindingProxyNames(rs)
				if len(names) == 0 {
					continue
				}
				probeProxies(ctx, proxyStore, proxyLatency, names, proxyTestHost, proxyTestPort)
			}
		}
	}()

	// App watcher: 1s tick, but the expensive lsof scan only runs when
	// the set of utun interfaces changes. net.Interfaces() is a cheap
	// kernel call (~0 CPU); lsof -nP +c 0 scans all process FDs and is
	// the primary cause of CPU hotspots when run every second.
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var lastSig string
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sig := utunSignature()
				if sig == lastSig {
					continue // no utun change — skip lsof
				}
				lastSig = sig
				changes := apps.Refresh()
				for _, c := range changes {
					release := apps.AcquireForWrite(c.Key)
					if c.Old != "" {
						_ = router.RemoveByInterface(ctx, c.Old)
					}
					log.Printf("em-walld: app %s: %s → %s", c.Key,
						orDash(c.Old), orDash(c.New))
					release()
				}
			}
		}
	}()

	// Xray log-cap watcher: every minute, check whether xray's access/
	// error log files have crossed the cap; restart xray to truncate
	// them if so. Restart also runs unconditionally on every config
	// change via Reconcile, so this is the only path that triggers a
	// restart purely for log-size reasons.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				xraySup.RotateLogsIfTooLarge()
			}
		}
	}()

	// Upstream watcher: every 10s, validate that the current upstream
	// still answers. When the user switches Wi-Fi, sleeps/wakes the
	// laptop, or toggles a VPN, the DHCP-supplied resolver we picked at
	// activation time is often no longer reachable — every query hangs
	// or NXDOMAIN's because we're forwarding into the void. The fix is
	// to notice and re-pick atomically: chooseUpstream reads
	// AllDHCPDNS, which queries `ipconfig getoption en0
	// domain_name_server` live, so it reflects whichever network we're
	// on now. Forwarder swap is atomic — no daemon restart, no
	// DNS-down window for the user.
	//
	// Same tick also retries activation: if the user wants the hijack
	// on but it isn't (typical: LaunchDaemon raced ahead of the network
	// at boot, so activateSystemDNS at startup couldn't validate any
	// upstream and safely restored DNS without flipping the pref),
	// re-attempt now that the network has had time to come up.
	go func() {
		defer wg.Done()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		// stableFor counts consecutive ticks where the upstream was healthy
		// and unchanged. Once it reaches the threshold we skip most checks,
		// only probing every skipEvery ticks (~60s) instead of every 10s.
		// Any failure or change resets it to 0 so we recover quickly.
		const skipEvery = 6 // 6 × 10s = 60s between probes when stable
		stableFor := 0
		skipCount := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if pref, _ := store.GetSetting(ctx, "system_dns_active", "true"); pref == "true" {
					if active, _ := deps.sysDNS.IsActive(); !active {
						if err := deps.activateSystemDNS(ctx); err != nil {
							log.Printf("em-walld: activation retry failed: %v", err)
						} else {
							log.Printf("em-walld: system DNS hijack activated on retry")
						}
						stableFor = 0
						skipCount = 0
						continue
					}
				}
				// Back off DNS health probes when stable: skip most ticks.
				if stableFor >= skipEvery {
					skipCount++
					if skipCount < skipEvery {
						continue
					}
					skipCount = 0
				}
				if changed, err := deps.refreshUpstreamIfStale(ctx); err != nil {
					log.Printf("em-walld: upstream refresh failed: %v", err)
					stableFor = 0
					skipCount = 0
				} else if changed {
					log.Printf("em-walld: upstream refreshed: now using %s", deps.upstream)
					stableFor = 0
					skipCount = 0
				} else {
					stableFor++
				}
			}
		}
	}()

	// Proxy tunnel: shuttle packets between the daemon-owned utun and
	// the netstack TCP stack. Only when the utun opened successfully
	// (needs root) — otherwise proxy: rules are unsupported and there's
	// nothing to serve. Start() blocks until ctx is cancelled or the
	// utun fd is closed by Stop().
	if proxyTunnel != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("em-walld: proxy tunnel serving on %s", proxyTunName)
			proxyTunnel.Start(ctx)
		}()
	}

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("em-walld: shutting down")

	cancel()
	dnsServer.Shutdown()
	ipcSrv.Shutdown()
	if proxyTunnel != nil {
		// Closes the utun fd, which unblocks the read loop (ctx alone
		// can't — it's parked in a blocking Read). Idempotent with the
		// deferred Stop().
		proxyTunnel.Stop()
	}
	router.Flush(context.Background())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("em-walld: shutdown timeout")
	}
	log.Printf("em-walld: bye")
}

type storeLogSink struct {
	store *rules.Store
}

func (s *storeLogSink) Log(name, action, iface string, ruleID int64, clientIP string) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.store.Log(ctx, rules.LogEntry{
		QueryName: name,
		Action:    action,
		RuleID:    ruleID,
		Interface: iface,
		ClientIP:  clientIP,
	})
}

// ---------- IPC handler wiring ----------

type handlerDeps struct {
	store       *rules.Store
	proxyStore  *proxy.Store
	proxyTable  *proxy.Table
	xrayStore   *xray.Store
	xraySup     *xraySupervisor
	xrayDataDir string // path to dir containing geoip.dat + geosite.dat
	engine      *decision.Engine
	router      *routing.Manager
	pf          *pfctl.Manager
	sysDNS      *SystemDNS
	dnsServer   *dnsproxy.Server
	apps        *applocator.Resolver
	listenAddr  string
	upstream    string
	startedAt   time.Time

	// Reachability-probe target for proxies.test, parsed from the
	// -proxy-test-target flag. host may be an IP literal or a DNS name.
	proxyTestHost string
	proxyTestPort int

	mu sync.Mutex // guards upstream
}

// loadUpstream picks the daemon's startup forwarder list. Loopback is
// stripped at every layer — under no circumstance should we forward
// queries to ourselves and create a tight loop. The picked list is
// validated lazily by activateSystemDNS before being applied.
//
// Order:
//  1. settings.upstream_dns (set by Activate, captured pre-hijack)
//  2. flagDefault (CLI flag)
//  3. AllDHCPDNS across every non-tunnel hardware port
//  4. scutil --dns (excluding loopback)
//  5. public fallback (1.1.1.1, 8.8.8.8) — last resort
func loadUpstream(store *rules.Store, flagDefault string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if v, _ := store.GetSetting(ctx, "upstream_dns", ""); v != "" {
		if clean := stripLoopback(splitCSV(v)); len(clean) > 0 {
			return clean
		}
	}
	if flagDefault != "" {
		if clean := stripLoopback(splitCSV(flagDefault)); len(clean) > 0 {
			return clean
		}
	}
	sd := NewSystemDNS(nil)
	if ips, err := sd.AllDHCPDNS(); err == nil && len(ips) > 0 {
		return WithPort53(ips)
	}
	if ips, err := sd.DetectResolvers(); err == nil && len(ips) > 0 {
		return WithPort53(ips)
	}
	return publicFallbackDNS
}

// stripLoopback drops 127.* / ::1 entries from a list of host[:port]
// strings. Empty list → empty list (caller decides fallback).
func stripLoopback(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a
		}
		if isLoopback(host) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinCSV(parts []string) string { return strings.Join(parts, ",") }

// parseProxyTestTarget splits a "host:port" string into its parts for
// the proxies.test reachability probe. host may be an IP or a DNS
// name. On any parse error (missing port, non-numeric port, etc.) it
// logs and falls back to the default target — the test feature is
// non-critical, so a bad flag shouldn't block daemon startup.
func parseProxyTestTarget(s string) (host string, port int) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(s))
	if err == nil {
		if n, perr := strconv.Atoi(p); perr == nil && n >= 1 && n <= 65535 && h != "" {
			return h, n
		}
	}
	log.Printf("em-walld: invalid -proxy-test-target %q, using default %q", s, defaultProxyTestTarget)
	dh, dp, _ := net.SplitHostPort(defaultProxyTestTarget)
	n, _ := strconv.Atoi(dp)
	return dh, n
}

// parseTestTarget is the per-call counterpart used by xray.test: a
// user-supplied "host:port" with sensible fallback when the field is
// blank. Returns an error (not a fallback) on a malformed non-empty
// value so the UI can surface "invalid target: ..." instead of
// silently dialing the wrong place.
func parseTestTarget(s, fallbackHost string, fallbackPort int) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallbackHost, fallbackPort, nil
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, err
	}
	n, perr := strconv.Atoi(p)
	if perr != nil || n < 1 || n > 65535 || h == "" {
		return "", 0, fmt.Errorf("port out of range or host empty")
	}
	return h, n, nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// utunSignature returns a comma-joined sorted list of current utun
// interface names. Changes iff VPN tunnels appear or disappear — used
// as a cheap sentinel to gate the expensive lsof scan.
func utunSignature() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var names []string
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "utun") {
			names = append(names, iface.Name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
