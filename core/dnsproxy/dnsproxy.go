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

// LogSink receives one entry per non-allow decision (block + route).
type LogSink interface {
	Log(name, action, iface string, ruleID int64, clientIP string)
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
	Logs        LogSink
	Logger      *log.Logger
}

type Server struct {
	cfg Config

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
	return &Server{cfg: cfg}, nil
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
		iface, release, ok := s.resolveIface(d.Interface, name, d.RuleID, clientIP, w, req)
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
		if strings.HasPrefix(d.Interface, "app:") {
			logIface = d.Interface + " → " + iface
		}
		if s.cfg.Interfaces != nil && !s.cfg.Interfaces.IsUp(iface) {
			s.writeNX(w, req, name)
			s.log(name, "block-iface-down", logIface, d.RuleID, clientIP)
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
//   - empty                   → "" (caller treats as default route)
//   - "utunN" / "enN"         → returned as-is
//   - "app:<k1>[,<k2>,...]"   → resolved via the AppLocator. Walks
//     the candidate list in order and uses the first running app's
//     utun. Acquires the per-app read lock (waits up to AppHoldMax).
//     The caller MUST invoke release() after writing the response
//     and installing any routes.
//
// Returns ok=false if a response has already been written (no apps
// running, lock timeout, …) — the caller must stop processing.
func (s *Server) resolveIface(stored, qname string, ruleID int64, clientIP string, w dns.ResponseWriter, req *dns.Msg) (iface string, release func(), ok bool) {
	noop := func() {}
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
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
		if err := s.cfg.Routes.Install(ctx, ip.String(), iface, life, ruleID); err != nil {
			s.cfg.Logger.Printf("dnsproxy: route install %s via %s failed: %v", ip, iface, err)
			return err
		}
	}
	return nil
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

func (m *MultiUpstream) Forward(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	var lastErr error
	for _, srv := range m.Servers {
		resp, _, err := m.udpClient.ExchangeContext(ctx, msg, srv)
		if err == nil && resp != nil && !resp.Truncated {
			return resp, nil
		}
		if resp != nil && resp.Truncated {
			resp, _, err = m.tcpClient.ExchangeContext(ctx, msg, srv)
			if err == nil && resp != nil {
				return resp, nil
			}
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dnsproxy: no upstream answered")
	}
	return nil, lastErr
}
