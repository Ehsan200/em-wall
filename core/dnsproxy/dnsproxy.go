// Package dnsproxy is a small recursive-style DNS server. It listens
// on UDP+TCP, evaluates each query against a decision.Engine, and
// either returns NXDOMAIN, forwards upstream, or forwards-and-routes.
package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

// Forwarder asks an upstream DNS server. Production uses MultiUpstream;
// tests inject a mock.
type Forwarder interface {
	Forward(ctx context.Context, msg *dns.Msg) (*dns.Msg, error)
}

// RouteInstaller is satisfied by *routing.Manager. We depend only on
// the verbs we need so this package can be tested without root.
type RouteInstaller interface {
	Install(ctx context.Context, host, iface string, ttl time.Duration, ruleID int64) error
}

// InterfaceChecker reports whether a network interface exists and is up.
// Used to enforce strict allow-via-iface: if the chosen interface is
// missing/down we refuse to resolve, so apps can't fall back to the
// default route.
type InterfaceChecker interface {
	IsUp(name string) bool
}

// AppLocator resolves an app key to the utun it currently owns and
// holds a per-app read lock for the duration of a query, so that a
// concurrent transition (utun number change) doesn't strand the
// query with a stale interface. nil = no app: prefix support.
//
// FirstAvailable picks the first running app from a candidate list —
// used to support multi-app rules ("via v2box OR Hiddify").
type AppLocator interface {
	Current(appKey string) string
	FirstAvailable(keys []string) (key, iface string)
	AcquireForRead(appKey string, timeout time.Duration) (release func(), ok bool)
}

type netInterfaceChecker struct{}

func (netInterfaceChecker) IsUp(name string) bool {
	if name == "" {
		return true
	}
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	return ifc.Flags&net.FlagUp != 0
}

// DefaultInterfaceChecker is the production implementation, exported
// so the daemon can pass it explicitly.
var DefaultInterfaceChecker InterfaceChecker = netInterfaceChecker{}

// routeInstallTimeout caps a single per-host route install. Applied
// per-IP (not per-answer) so a multi-record response can't starve its
// later IPs. Generous enough to absorb routing.Manager's internal retries.
const routeInstallTimeout = 3 * time.Second

// LogSink receives one entry per non-allow decision (block + route).
type LogSink interface {
	Log(name, action, iface string, ruleID int64, clientIP string)
}

// ProxyResolver translates a "proxy:NAME[,NAME2,...]" interface field
// into a concrete upstream-proxy choice, and records the
// IP → (proxyNames, hostname) mapping that the netstack handler
// consults later. nil = no "proxy:" prefix support (rules using it
// degrade to NXDOMAIN with action "block-proxy-unsupported").
//
// HasProxy is a cheap existence check used during resolution; the
// daemon's implementation reads from core/proxy.Store. We don't do
// live reachability here — that's a Phase B+ refinement — so a proxy
// whose upstream is down still "exists" until the user deletes it.
type ProxyResolver interface {
	HasProxy(name string) bool
	Record(ip net.IP, hostname string, proxyNames []string, ttl time.Duration, ruleID int64)
}

type Config struct {
	Listen      string        // e.g. "127.0.0.1:53"
	NegativeTTL uint32        // TTL on NXDOMAIN responses
	RouteTTLMin time.Duration // floor on per-host route lifetime
	AppHoldMax  time.Duration // max wait for an app's read lock during transitions (default 2s)
	Decider     *decision.Engine
	Forwarder   Forwarder
	Routes      RouteInstaller
	Interfaces  InterfaceChecker // nil → no enforcement (allow-via-iface won't strictly enforce)
	Apps        AppLocator       // nil → no `app:` prefix support
	Proxies     ProxyResolver    // nil → no `proxy:` prefix support
	// EnableFakeIP turns on fake-IP for proxy:/xray: routed names: instead
	// of resolving upstream, the server hands the client a synthetic IP
	// from FakeIPv4CIDR, pins it to the proxy utun, and records the
	// hostname so the netstack layer redials it through the proxy. nil
	// FakeIP pool (see New) when false. Requires Proxies + ProxyTun.
	EnableFakeIP bool
	// FakeIPv4CIDR is the reserved range fake IPs are drawn from. Empty →
	// DefaultFakeIPv4CIDR (198.18.0.0/15, RFC 2544 benchmarking, never
	// globally routed so it can't collide with real destinations).
	FakeIPv4CIDR string
	// ProxyTun is the kernel-assigned name of the daemon-owned utun
	// where the user-space TCP stack accepts proxy-routed connections.
	// Empty disables the "proxy:" path; resolveIface will treat such
	// rules as unsupported.
	ProxyTun string
	Logs     LogSink
	Logger   *log.Logger
}

type Server struct {
	cfg Config

	// fakeIP is non-nil only when EnableFakeIP is set and the pool built
	// successfully. Drives the proxy:/xray: fake-IP fast path.
	fakeIP *fakeIPPool

	mu      sync.Mutex
	udp     *dns.Server
	tcp     *dns.Server
	ready   chan struct{}
	readyN  int
	readyMu sync.Mutex
}

// Ready returns a channel that closes when both listeners are up.
func (s *Server) Ready() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	return s.ready
}

func (s *Server) markListenerReady() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	s.readyN++
	if s.readyN == 2 && s.ready != nil {
		close(s.ready)
	}
}

func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:53"
	}
	if cfg.NegativeTTL == 0 {
		cfg.NegativeTTL = 60
	}
	if cfg.RouteTTLMin == 0 {
		cfg.RouteTTLMin = 30 * time.Second
	}
	if cfg.AppHoldMax == 0 {
		cfg.AppHoldMax = 2 * time.Second
	}
	if cfg.Decider == nil {
		return nil, errors.New("dnsproxy: missing Decider")
	}
	if cfg.Forwarder == nil {
		return nil, errors.New("dnsproxy: missing Forwarder")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	s := &Server{cfg: cfg}
	if cfg.EnableFakeIP {
		cidr := cfg.FakeIPv4CIDR
		if cidr == "" {
			cidr = DefaultFakeIPv4CIDR
		}
		pool, err := newFakeIPPool(cidr, cfg.RouteTTLMin)
		if err != nil {
			return nil, fmt.Errorf("dnsproxy: fake-ip pool: %w", err)
		}
		s.fakeIP = pool
	}
	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	handler := dns.HandlerFunc(s.handle)
	s.mu.Lock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	s.udp = &dns.Server{Addr: s.cfg.Listen, Net: "udp", Handler: handler, NotifyStartedFunc: s.markListenerReady}
	s.tcp = &dns.Server{Addr: s.cfg.Listen, Net: "tcp", Handler: handler, NotifyStartedFunc: s.markListenerReady}
	s.mu.Unlock()

	errc := make(chan error, 2)
	go func() { errc <- s.udp.ListenAndServe() }()
	go func() { errc <- s.tcp.ListenAndServe() }()

	select {
	case <-ctx.Done():
		s.Shutdown()
		return nil
	case err := <-errc:
		s.Shutdown()
		return err
	}
}

func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udp != nil {
		_ = s.udp.Shutdown()
	}
	if s.tcp != nil {
		_ = s.tcp.Shutdown()
	}
}

// handle is the hot path. Keep it small; do real work in helpers.
func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 0 {
		return
	}
	q := req.Question[0]
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
	clientIP := remoteIP(w)

	d := s.cfg.Decider.Decide(name)

	switch d.Outcome {
	case decision.OutcomeBlock:
		s.writeNX(w, req, name)
		s.log(name, "block", "", d.RuleID, clientIP)
		return

	case decision.OutcomeRoute:
		// Translate xray:NAME into its internal proxy:_xray_NAME form
		// so all the proxy-routing plumbing below (resolveIface,
		// recordProxyMapping) sees a single shape. d.Interface stays
		// the user-facing label and is used in logs verbatim.
		effIface := translateXrayInterface(d.Interface)
		iface, release, ok := s.resolveIface(effIface, name, d.RuleID, clientIP, w, req)
		if !ok {
			return // resolveIface already wrote a response and logged
		}
		defer release()
		// Build the log-friendly interface label up front so EVERY
		// failure branch (iface-down, forward-failed, route-failed)
		// AND the success branch use the same `app:X → utunN` form.
		// Without this, app-bound failures looked like fixed-iface
		// failures in the Logs tab.
		logIface := iface
		if strings.HasPrefix(d.Interface, "app:") || proxy.IsProxyInterface(d.Interface) || xray.IsXrayInterface(d.Interface) {
			logIface = d.Interface + " → " + iface
		}
		if s.cfg.Interfaces != nil && !s.cfg.Interfaces.IsUp(iface) {
			s.writeNX(w, req, name)
			s.log(name, "block-iface-down", logIface, d.RuleID, clientIP)
			return
		}
		// Fake-IP fast path for proxy:/xray: routes. We never resolve the
		// name upstream: the real lookup happens proxy-side at connect time
		// (the netstack handler redials by hostname), so the IP we return is
		// only a routing handle. Synthesizing it avoids leaking the user's
		// network to the answer's geo and sidesteps the region-pinned CDN
		// hostnames (googlevideo `sn-*`) that SERVFAIL on a local/VPN
		// resolver and used to surface as `forward-failed`.
		if s.fakeIP != nil && (proxy.IsProxyInterface(d.Interface) || xray.IsXrayInterface(d.Interface)) {
			s.handleFakeIP(w, req, name, iface, effIface, logIface, d.RuleID, clientIP)
			return
		}
		resp, err := s.forward(req)
		if err != nil {
			s.cfg.Logger.Printf("dnsproxy: forward failed for %s: %v", name, err)
			s.writeServFail(w, req)
			s.log(name, "forward-failed", logIface, d.RuleID, clientIP)
			return
		}
		// Fail-closed: if we can't pin even ONE answer to the chosen
		// interface, do NOT deliver the response — otherwise the OS
		// would route the leaked IP via the default gateway.
		if err := s.installRoutesFor(resp, iface, d.RuleID); err != nil {
			s.writeNX(w, req, name)
			s.log(name, "block-route-failed", logIface, d.RuleID, clientIP)
			return
		}
		// For proxy-routed answers, also record the per-IP mapping so
		// the netstack TCP handler knows which upstream proxy to use
		// when the client subsequently connects. installRoutesFor
		// already pinned the IP to our utun; now we annotate that
		// pin with (proxyNames, hostname).
		// recordProxyMapping reads the proxy names out of the stored
		// interface, so pass effIface (which has xray:NAME already
		// rewritten to its internal proxy form) — otherwise xray-
		// routed answers would land in the table with the raw xray:
		// names and the netstack handler couldn't dial them.
		if (proxy.IsProxyInterface(d.Interface) || xray.IsXrayInterface(d.Interface)) && s.cfg.Proxies != nil {
			s.recordProxyMapping(resp, name, effIface, d.RuleID)
		}
		_ = w.WriteMsg(resp)
		s.log(name, "route", logIface, d.RuleID, clientIP)
		return

	case decision.OutcomeAllow:
		fallthrough
	default:
		resp, err := s.forward(req)
		if err != nil {
			s.cfg.Logger.Printf("dnsproxy: forward failed for %s: %v", name, err)
			s.writeServFail(w, req)
			return
		}
		_ = w.WriteMsg(resp)
		// Plain allows are not logged per user spec.
	}
}

// resolveIface translates the rule's stored interface field into a
// concrete interface name. Cases:
//
//   - empty                       → "" (caller treats as default route)
//   - "utunN" / "enN"             → returned as-is
//   - "app:<k1>[,<k2>,...]"       → resolved via the AppLocator. Walks
//     the candidate list in order and uses the first running app's
//     utun. Acquires the per-app read lock (waits up to AppHoldMax).
//     The caller MUST invoke release() after writing the response
//     and installing any routes.
//   - "proxy:<n1>[,<n2>,...]"     → resolved via the ProxyResolver.
//     Returns the daemon-owned utun (cfg.ProxyTun) so per-host routes
//     are pinned there. The TCP layer (proxytun + netstack) then
//     dispatches per-connection through the chosen upstream proxy.
//     No locking needed — proxy records are static config, not
//     transient like app utun assignments.
//
// Returns ok=false if a response has already been written (no apps
// running, lock timeout, …) — the caller must stop processing.
func (s *Server) resolveIface(stored, qname string, ruleID int64, clientIP string, w dns.ResponseWriter, req *dns.Msg) (iface string, release func(), ok bool) {
	noop := func() {}
	if proxy.IsProxyInterface(stored) {
		if s.cfg.Proxies == nil || s.cfg.ProxyTun == "" {
			s.writeServFail(w, req)
			s.log(qname, "block-proxy-unsupported", stored, ruleID, clientIP)
			return "", noop, false
		}
		names := proxy.ParseInterface(stored)
		if len(names) == 0 {
			s.writeNX(w, req, qname)
			s.log(qname, "block-proxy-missing", stored, ruleID, clientIP)
			return "", noop, false
		}
		// Pick the first proxy whose record still exists. We don't do
		// live reachability here — if the upstream is down the TCP
		// splice will fail and the client sees a connection reset.
		// Rule-time validation already rejected unknown names, but a
		// proxy can be deleted between then and now.
		picked := ""
		for _, n := range names {
			if s.cfg.Proxies.HasProxy(n) {
				picked = n
				break
			}
		}
		if picked == "" {
			s.writeNX(w, req, qname)
			s.log(qname, "block-proxy-missing", stored, ruleID, clientIP)
			return "", noop, false
		}
		return s.cfg.ProxyTun, noop, true
	}
	if !strings.HasPrefix(stored, "app:") {
		return stored, noop, true
	}
	if s.cfg.Apps == nil {
		s.writeServFail(w, req)
		s.log(qname, "block-app-unsupported", stored, ruleID, clientIP)
		return "", noop, false
	}

	keys := parseAppKeys(stored)
	if len(keys) == 0 {
		s.writeNX(w, req, qname)
		s.log(qname, "block-app-down", stored, ruleID, clientIP)
		return "", noop, false
	}

	pickedKey, pickedIface := s.cfg.Apps.FirstAvailable(keys)
	if pickedKey == "" {
		s.writeNX(w, req, qname)
		s.log(qname, "block-app-down", stored, ruleID, clientIP)
		return "", noop, false
	}

	rel, gotLock := s.cfg.Apps.AcquireForRead(pickedKey, s.cfg.AppHoldMax)
	if !gotLock {
		s.writeNX(w, req, qname)
		s.log(qname, "block-app-busy", stored, ruleID, clientIP)
		return "", noop, false
	}

	// Re-check after acquiring the lock — a concurrent transition may
	// have just torn down the app's utun.
	current := s.cfg.Apps.Current(pickedKey)
	if current == "" {
		rel()
		s.writeNX(w, req, qname)
		s.log(qname, "block-app-down", stored, ruleID, clientIP)
		return "", noop, false
	}
	if current != pickedIface {
		// Utun number changed between FirstAvailable and lock — use
		// the post-lock value, which is the canonical one.
		pickedIface = current
	}
	return pickedIface, rel, true
}

// parseAppKeys parses "app:k1,k2,k3" into ["k1","k2","k3"], dropping
// empty entries and trimming whitespace.
func parseAppKeys(stored string) []string {
	raw := strings.TrimPrefix(stored, "app:")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// translateXrayInterface rewrites a stored "xray:NAME[,NAME2]" field
// into the equivalent "proxy:_xray_NAME[,_xray_NAME2]" so the rest of
// the routing layer doesn't need to know about xray. Non-xray inputs
// are returned unchanged.
//
// The supervisor keeps a hidden proxy.Proxy row named _xray_NAME
// (Host=127.0.0.1, Port=entry SocksPort, Protocol=socks5) for every
// xray entry, so once rewritten the existing proxy plumbing dials
// the right local SOCKS5 inbound.
func translateXrayInterface(stored string) string {
	if !xray.IsXrayInterface(stored) {
		return stored
	}
	names := xray.ParseInterface(stored)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = xray.InternalProxyName(n)
	}
	return proxy.InterfacePrefix + strings.Join(parts, ",")
}

func (s *Server) writeNX(w dns.ResponseWriter, req *dns.Msg, name string) {
	resp := new(dns.Msg)
	resp.SetRcode(req, dns.RcodeNameError)
	// Synthesize a SOA for negative caching TTL. RFC 2308 §5.
	soa := &dns.SOA{
		Hdr: dns.RR_Header{
			Name:   dns.Fqdn(parentDomain(name)),
			Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: s.cfg.NegativeTTL,
		},
		Ns:     "em-wall.invalid.",
		Mbox:   "em-wall.invalid.",
		Serial: 1, Refresh: 0, Retry: 0, Expire: 0, Minttl: s.cfg.NegativeTTL,
	}
	resp.Ns = []dns.RR{soa}
	_ = w.WriteMsg(resp)
}

func (s *Server) writeServFail(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetRcode(req, dns.RcodeServerFailure)
	_ = w.WriteMsg(resp)
}

// writeEmpty replies NOERROR with no answers (NODATA) plus a negative-cache
// SOA. Used by the fake-IP path for AAAA/HTTPS/etc. on a proxy-routed name:
// we only hand out a fake A, so steering the client to it means saying
// "this name exists but has no record of that type" for everything else.
func (s *Server) writeEmpty(w dns.ResponseWriter, req *dns.Msg, name string) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	soa := &dns.SOA{
		Hdr: dns.RR_Header{
			Name:   dns.Fqdn(parentDomain(name)),
			Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: s.cfg.NegativeTTL,
		},
		Ns: "em-wall.invalid.", Mbox: "em-wall.invalid.",
		Serial: 1, Minttl: s.cfg.NegativeTTL,
	}
	resp.Ns = []dns.RR{soa}
	_ = w.WriteMsg(resp)
}

// handleFakeIP serves a proxy:/xray: routed query without any upstream
// lookup. For an A question it allocates a fake IP for the hostname, pins it
// to the proxy utun, records the (proxyNames, hostname) mapping the netstack
// handler consults at connect time, and answers with that synthetic record.
// Every other qtype gets NODATA so the client falls through to the A record.
// effIface is the xray-translated interface so the recorded proxy names match
// what the TCP layer dials.
func (s *Server) handleFakeIP(w dns.ResponseWriter, req *dns.Msg, name, iface, effIface, logIface string, ruleID int64, clientIP string) {
	q := req.Question[0]
	if q.Qtype != dns.TypeA {
		// AAAA/HTTPS/etc.: NODATA. The proxy decides the egress address
		// family itself when it dials the hostname, so withholding a fake
		// AAAA just steers the client onto our fake A — no v6 fake range,
		// no v6 host routes to manage.
		s.writeEmpty(w, req, name)
		s.log(name, "route", logIface, ruleID, clientIP)
		return
	}

	ip, ttl, ok := s.fakeIP.Allocate(name)
	if !ok {
		// Pool exhausted — fail closed rather than leak via the default
		// route. Extremely unlikely with a /15 (131072 slots).
		s.writeServFail(w, req)
		s.log(name, "block-fakeip-exhausted", logIface, ruleID, clientIP)
		return
	}

	// Pin the fake IP to the proxy utun so the client's connection lands on
	// our netstack. Fail closed if the route won't install.
	if s.cfg.Routes != nil {
		ctx, cancel := context.WithTimeout(context.Background(), routeInstallTimeout)
		err := s.cfg.Routes.Install(ctx, ip.String(), iface, ttl, ruleID)
		cancel()
		if err != nil {
			// Hand the slot back so a transient route failure doesn't leak
			// a lease until expiry.
			s.fakeIP.Release(name)
			s.cfg.Logger.Printf("dnsproxy: fake-ip route %s via %s failed: %v", ip, iface, err)
			s.writeServFail(w, req)
			s.log(name, "block-route-failed", logIface, ruleID, clientIP)
			return
		}
	}
	// Record IP → (proxyNames, hostname) so the netstack handler redials the
	// real hostname through the proxy when the client connects to the fake IP.
	if s.cfg.Proxies != nil {
		names := proxy.ParseInterface(effIface)
		s.cfg.Proxies.Record(ip, name, names, ttl, ruleID)
	}

	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeA, Class: dns.ClassINET,
			Ttl: uint32(ttl / time.Second),
		},
		A: ip,
	}}
	_ = w.WriteMsg(resp)
	s.log(name, "route", logIface, ruleID, clientIP)
}

func (s *Server) forward(req *dns.Msg) (*dns.Msg, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	fwd := s.cfg.Forwarder
	s.mu.Unlock()
	resp, err := fwd.Forward(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("dnsproxy: nil upstream response")
	}
	resp.Id = req.Id
	return resp, nil
}

// SetForwarder swaps the upstream forwarder at runtime. Existing
// in-flight queries continue using whichever forwarder they captured.
func (s *Server) SetForwarder(f Forwarder) {
	if f == nil {
		return
	}
	s.mu.Lock()
	s.cfg.Forwarder = f
	s.mu.Unlock()
}

// installRoutesFor pins each A/AAAA from resp to iface. Returns an
// error if ANY install fails — caller MUST treat this as fail-closed
// (return NXDOMAIN, do not deliver the response). Without this, a
// bogus interface (e.g. "app:tailscale" resolved to a non-existent
// utun in old code paths) would let `route add` fail silently and
// the IP would leak via the default route.
func (s *Server) installRoutesFor(resp *dns.Msg, iface string, ruleID int64) error {
	if s.cfg.Routes == nil || iface == "" {
		return nil
	}
	for _, rr := range resp.Answer {
		var ip net.IP
		var ttl uint32
		switch v := rr.(type) {
		case *dns.A:
			ip, ttl = v.A, v.Hdr.Ttl
		case *dns.AAAA:
			ip, ttl = v.AAAA, v.Hdr.Ttl
		default:
			continue
		}
		if ip == nil {
			continue
		}
		life := time.Duration(ttl) * time.Second
		if life < s.cfg.RouteTTLMin {
			life = s.cfg.RouteTTLMin
		}
		// Per-IP timeout: a multi-answer response (e.g. Google's several
		// A records) must not share one budget where slow early installs
		// starve later IPs into a fail-closed whole-answer drop.
		ctx, cancel := context.WithTimeout(context.Background(), routeInstallTimeout)
		err := s.cfg.Routes.Install(ctx, ip.String(), iface, life, ruleID)
		cancel()
		if err != nil {
			s.cfg.Logger.Printf("dnsproxy: route install %s via %s failed: %v", ip, iface, err)
			return err
		}
	}
	return nil
}

// recordProxyMapping annotates each A/AAAA IP in resp with the proxy
// binding so the netstack TCP handler can dispatch the connection to
// the right upstream proxy when the client subsequently connects.
// Mirrors installRoutesFor's answer walk — the route pin (IP → our
// utun) and this proxy mapping (IP → proxyNames, hostname) are the two
// halves of the same "this IP is proxy-routed" fact, so they use the
// same TTL floor. hostname is the queried name, recorded so the
// upstream CONNECT/SOCKS request preserves it for SNI and proxy-side
// hostname ACLs instead of dialing a bare IP.
func (s *Server) recordProxyMapping(resp *dns.Msg, hostname, stored string, ruleID int64) {
	names := proxy.ParseInterface(stored)
	if len(names) == 0 {
		return
	}
	for _, rr := range resp.Answer {
		var ip net.IP
		var ttl uint32
		switch v := rr.(type) {
		case *dns.A:
			ip, ttl = v.A, v.Hdr.Ttl
		case *dns.AAAA:
			ip, ttl = v.AAAA, v.Hdr.Ttl
		default:
			continue
		}
		if ip == nil {
			continue
		}
		life := time.Duration(ttl) * time.Second
		if life < s.cfg.RouteTTLMin {
			life = s.cfg.RouteTTLMin
		}
		s.cfg.Proxies.Record(ip, hostname, names, life, ruleID)
	}
}

func (s *Server) log(name, action, iface string, ruleID int64, clientIP string) {
	if s.cfg.Logs == nil {
		return
	}
	s.cfg.Logs.Log(name, action, iface, ruleID, clientIP)
}

func remoteIP(w dns.ResponseWriter) string {
	a := w.RemoteAddr()
	if a == nil {
		return ""
	}
	switch x := a.(type) {
	case *net.UDPAddr:
		return x.IP.String()
	case *net.TCPAddr:
		return x.IP.String()
	}
	return a.String()
}

func parentDomain(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// MultiUpstream forwards to a list of upstream resolvers in order until
// one answers. Each upstream is "host:port".
type MultiUpstream struct {
	Servers []string
	Timeout time.Duration

	udpClient *dns.Client
	tcpClient *dns.Client
}

func NewMultiUpstream(servers []string, timeout time.Duration) *MultiUpstream {
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	return &MultiUpstream{
		Servers:   servers,
		Timeout:   timeout,
		udpClient: &dns.Client{Net: "udp", Timeout: timeout},
		tcpClient: &dns.Client{Net: "tcp", Timeout: timeout},
	}
}

// Forward sends msg to each upstream and returns the BEST answer by
// outcome rank — not the first reply. A NOERROR-with-answers from any
// upstream beats an NXDOMAIN/NODATA from another, which beats
// SERVFAIL/REFUSED/transport errors.
//
// Ranking (rather than first-reply-wins) is what makes split / tampered
// DNS resolve correctly. A foreign public resolver returns NXDOMAIN for a
// geo-split or country-code name — e.g. an .ir host whose authoritative
// servers only answer in-country, or an on-path box that injects NXDOMAIN —
// while the ISP/local resolver (also in our upstream list) resolves it. The
// old code treated the first NXDOMAIN as definitive and stopped, so the
// client got "no answer" even though a resolver that knew the name was one
// entry away. Ranking lets that resolver win regardless of upstream order.
//
// We stop the moment any upstream gives a positive (has-answers) reply.
// SERVFAIL/REFUSED and transport errors are SOFT: if we never get better
// than a soft failure we retry the whole list once (cold recursive cache or
// a slow path to distant authoritative servers — usually transient). A
// definitive negative (NXDOMAIN/NODATA) is returned without a second pass.
func (m *MultiUpstream) Forward(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	var best *dns.Msg
	bestRank := rankNone
	var lastErr error

	attempt := func() (gotPositive bool) {
		for _, srv := range m.Servers {
			resp, err := m.exchange(ctx, srv, msg)
			if err != nil {
				lastErr = err
				continue
			}
			if resp == nil {
				continue
			}
			if r := rankResponse(resp); r > bestRank {
				best, bestRank = resp, r
				if r == rankPositive {
					return true // a real answer — nothing outranks it
				}
			}
		}
		return false
	}

	if attempt() {
		return best, nil
	}
	// Retry only when we have nothing better than a soft failure; a
	// definitive NXDOMAIN/NODATA won't improve by asking again.
	if bestRank <= rankSoftFail {
		if attempt() {
			return best, nil
		}
	}
	if best != nil {
		return best, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dnsproxy: no upstream answered")
	}
	return nil, lastErr
}

// Response outcome ranks, worst to best, used by Forward to choose the most
// useful reply across upstreams instead of the first one to arrive.
const (
	rankNone     = iota // no usable response (transport error / nil)
	rankSoftFail        // SERVFAIL / REFUSED / other server error
	rankNXDomain        // authoritative "name does not exist"
	rankNoData          // NOERROR with no answers ("exists, no such record")
	rankPositive        // NOERROR with >=1 answer record
)

func rankResponse(resp *dns.Msg) int {
	switch resp.Rcode {
	case dns.RcodeSuccess:
		if len(resp.Answer) > 0 {
			return rankPositive
		}
		return rankNoData
	case dns.RcodeNameError:
		return rankNXDomain
	default:
		return rankSoftFail
	}
}

// exchange queries srv over UDP, retrying over TCP if the reply was
// truncated. Returns the response (which may carry any Rcode) and any
// transport error.
func (m *MultiUpstream) exchange(ctx context.Context, srv string, msg *dns.Msg) (*dns.Msg, error) {
	resp, _, err := m.udpClient.ExchangeContext(ctx, msg, srv)
	if err == nil && resp != nil && resp.Truncated {
		resp, _, err = m.tcpClient.ExchangeContext(ctx, msg, srv)
	}
	return resp, err
}
