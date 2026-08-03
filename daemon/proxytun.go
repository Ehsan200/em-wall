package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/proxytun"
	"github.com/ehsan/em-wall/core/routing"
	"github.com/ehsan/em-wall/core/xray"
)

// proxyUDPIdleTimeout tears down a UDP flow's association after this
// much silence in both directions. UDP has no FIN, so this is the only
// way to reclaim a flow's relay socket + control conn. Long enough for
// chatty QUIC, short enough that one-shot DNS lookups don't linger.
const proxyUDPIdleTimeout = 60 * time.Second

// proxyUDPNoReplyTimeout bounds how long a flow may send without ever hearing
// back. An association can come up dead — same host, same proxy, seconds
// apart, one carries traffic and one silently doesn't — and holding that one
// open just lets the client retransmit into it until it gives up (~90s for
// QUIC, which reads as "Gmail is slow"). Generous enough that a merely slow
// first reply isn't mistaken for a dead one.
//
// proxyUDPMonitorTick is how often a flow's watchdog re-evaluates. It has to
// be well under proxyUDPNoReplyTimeout to be useful, so byte-accounting
// flushes ride a counter rather than the tick itself.
//
// Both are vars rather than consts so tests can compress the timeline.
var (
	proxyUDPNoReplyTimeout = 6 * time.Second
	proxyUDPMonitorTick    = time.Second
)

// proxyUDPMaxReassoc caps how many times one flow may be moved to another
// association before we give up and let it die. Two is enough to clear a
// single bad association or a single bad upstream; beyond that the problem
// isn't the association and retrying just delays the client's TCP fallback.
const proxyUDPMaxReassoc = 2

// proxyUTUNAddr is the point-to-point address assigned to the daemon's
// utun. The address itself isn't routed — proxy-bound traffic reaches
// the utun via the per-host routes the DNS layer installs (by interface
// name), which are more specific than any VPN's default route — so this
// only needs to be an address nothing else claims.
//
// It must stay OUT of 198.18.0.0/15: that RFC 2544 range is exactly what
// fake-IP proxies (V2BOX, sing-box, Clash, v2ray) hand out, and a clash
// there means their fake-IPs and our tun fight over the same addresses
// (symptom: "no proxy mapping for 198.18.0.x; dropping"). TEST-NET-1
// (192.0.2.0/24, RFC 5737) is reserved for documentation and used by
// nothing in practice, so it's collision-free.
const proxyUTUNAddr = "192.0.2.1"

// Proxy dial retry tuning. One dial can fail transiently (handshake
// timeout, mid-handshake EOF, briefly overloaded relay) even when the
// proxy is healthy; without retry a single flake drops the connection,
// which on connection-heavy origins reads as slow/partial page loads. We
// retry the whole binding a few rounds with backoff, cap each dial so a
// hung proxy can't eat the budget, and bound the total via the outer ctx.
const (
	proxyDialAttemptTimeout = 6 * time.Second        // per single dial
	proxyDialMaxRounds      = 3                      // full-binding passes
	proxyDialBackoffBase    = 250 * time.Millisecond // *round between rounds
	proxyConnectDeadline    = 20 * time.Second       // hard cap, all rounds
)

// defaultProxyTestTarget is the endpoint the proxies.test handler dials
// through a proxy to confirm reachability, overridable via the
// -proxy-test-target flag. Cloudflare's 1.1.1.1:443 is a well-known
// always-on TCP endpoint; dialing by IP means the probe doesn't depend
// on proxy-side DNS resolution. Override with a hostname (e.g.
// "example.com:443") to also exercise the proxy's own DNS.
const defaultProxyTestTarget = "1.1.1.1:443"

// proxyResolver adapts proxy.Store + proxy.Table to the
// dnsproxy.ProxyResolver interface. Existence checks hit the store;
// IP→proxy mappings are written to the table for the netstack handler
// to read at connection time.
type proxyResolver struct {
	store *proxy.Store
	table *proxy.Table
}

func (r *proxyResolver) HasProxy(name string) bool {
	_, err := r.store.GetByName(context.Background(), name)
	return err == nil
}

func (r *proxyResolver) Record(ip net.IP, hostname string, names []string, ttl time.Duration, ruleID int64) {
	r.table.Record(ip, hostname, names, ttl, ruleID)
}

// proxyForwarder handles each TCP connection netstack accepts on the
// daemon's utun: look the destination IP up in the table, pick the
// first reachable upstream proxy from the rule's binding, dial it, and
// splice bytes both ways.
type proxyForwarder struct {
	store   *proxy.Store
	table   *proxy.Table
	router  *routing.Manager
	decider ipRouteDecider // resolves dest IPs with no DNS-time mapping; may be nil
	latency *netprobe.LatencyTracker
	traffic *trafficAggregator // nil disables byte accounting
	logger  *log.Logger
}

// ipRouteDecider resolves a destination IP to a routing decision for
// connections that arrive at the utun without a DNS-time table entry —
// i.e. traffic matched by an IP/CIDR rule rather than a domain rule.
// *decision.Engine satisfies it.
type ipRouteDecider interface {
	DecideIP(ip net.IP) decision.Decision
}

// proxyNamesForInterface resolves a rule's stored interface field
// ("proxy:NAME[,...]" or "xray:NAME[,...]") into the proxy.Store record
// names the dial path looks up. xray entries are backed by a hidden
// _xray_NAME proxy row (see translateXrayInterface in dnsproxy), so they
// map through xray.InternalProxyName. Non-proxy interfaces yield nil.
func proxyNamesForInterface(iface string) []string {
	if xray.IsXrayInterface(iface) {
		names := xray.ParseInterface(iface)
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, xray.InternalProxyName(n))
		}
		return out
	}
	return proxy.ParseInterface(iface)
}

// lookupIPRule resolves an IP/CIDR rule for dest when no DNS-time table
// entry exists. Returns a synthetic Entry for a proxy:/xray: route rule;
// false otherwise (no rule, non-route outcome, or a literal-interface rule
// whose traffic the OS already routed without going through netstack).
// Hostname is set to the destination IP — there's no DNS name to preserve,
// so the dial targets the IP directly and traffic accounts under it.
func (pf *proxyForwarder) lookupIPRule(dest net.IP) (proxy.Entry, bool) {
	if pf.decider == nil {
		return proxy.Entry{}, false
	}
	d := pf.decider.DecideIP(dest)
	if d.Outcome != decision.OutcomeRoute {
		return proxy.Entry{}, false
	}
	names := proxyNamesForInterface(d.Interface)
	if len(names) == 0 {
		return proxy.Entry{}, false
	}
	return proxy.Entry{ProxyNames: names, Hostname: dest.String(), RuleID: d.RuleID}, true
}

// proxyTrafficFlushInterval is how often an in-flight splice reports its
// byte deltas to the aggregator. Frequent enough that a long-lived stream
// shows up on the dashboard mid-connection, slow enough to stay cheap.
const proxyTrafficFlushInterval = 20 * time.Second

// recordTraffic forwards a sent/recv byte delta for host via proxy to the
// aggregator, if one is wired. sent = client→proxy, recv = proxy→client.
func (pf *proxyForwarder) recordTraffic(host, proxyName string, sent, recv int64) {
	if pf.traffic != nil {
		pf.traffic.Record(host, proxyName, sent, recv)
	}
}

// orderedNames returns the binding's proxy names lowest-latency first when a
// latency tracker is wired, else the binding's original order. Single-name
// bindings pass through untouched.
func (pf *proxyForwarder) orderedNames(entry proxy.Entry) []string {
	if pf.latency == nil {
		return entry.ProxyNames
	}
	return pf.latency.Rank(entry.ProxyNames)
}

// Route keepalive. An active flow doesn't re-query DNS, so its fake-IP host
// route would be swept mid-connection once the DNS-derived TTL lapses
// (sweep runs every 15s; route floor 30s) — the fake IP then blackholes and
// the connection dies. While a flow is live we re-stamp its route + table
// mapping expiry: immediately on connect, then on a ticker. The grant
// exceeds the sweep interval comfortably; once the flow ends, the last grant
// lapses and the route is reclaimed normally.
const (
	proxyRouteKeepaliveTTL      = 90 * time.Second
	proxyRouteKeepaliveInterval = 25 * time.Second
)

// keepRouteAlive stamps a fresh TTL on ip's route + mapping now, then keeps
// re-stamping on a ticker until the returned stop func is called. Safe with a
// nil router (dev daemon without proxy support).
func (pf *proxyForwarder) keepRouteAlive(ip net.IP) func() {
	pf.touchRoute(ip)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(proxyRouteKeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				pf.touchRoute(ip)
			}
		}
	}()
	return func() { close(done) }
}

func (pf *proxyForwarder) touchRoute(ip net.IP) {
	if pf.router != nil {
		pf.router.Extend(ip.String(), proxyRouteKeepaliveTTL)
	}
	pf.table.Touch(ip, proxyRouteKeepaliveTTL)
}

func (pf *proxyForwarder) handle(conn net.Conn, local, remote *net.TCPAddr) {
	defer conn.Close()

	entry, ok := pf.table.Lookup(local.IP)
	if !ok {
		// No DNS-time mapping. Either an IP/CIDR rule routed this real
		// dest IP to our utun (resolve it via the decision engine), or
		// it's a stale route lingering after a table sweep — drop then.
		entry, ok = pf.lookupIPRule(local.IP)
		if !ok {
			pf.logger.Printf("proxytun: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
			return
		}
	}

	target := entry.Hostname
	if target == "" {
		target = local.IP.String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
	defer cancel()

	upstream, used, lastErr := pf.dialBinding(ctx, entry, local)
	if upstream == nil {
		pf.logger.Printf("proxytun: %s:%d — all upstream proxies failed after %d rounds: %v", target, local.Port, proxyDialMaxRounds, lastErr)
		return
	}
	defer upstream.Close()

	// Keep the fake-IP route + mapping alive for as long as we're splicing,
	// so the TTL sweep can't cut this connection out from under us.
	stop := pf.keepRouteAlive(local.IP)
	defer stop()

	pf.logger.Printf("proxytun: %s:%d via proxy %q", target, local.Port, used)
	// conn→upstream is bytes the client sent; upstream→conn is bytes it
	// received. SpliceCounted reports deltas live so long-lived streams
	// register on the usage dashboard before they close.
	_, _, _ = proxy.SpliceCounted(conn, upstream, proxyTrafficFlushInterval, func(atob, btoa int64) {
		pf.recordTraffic(entry.Hostname, used, atob, btoa)
	})
}

// dialBinding walks the rule's proxy binding in order (first-available
// fallback) and retries the whole list with backoff on transient failure.
// Each dial is capped by proxyDialAttemptTimeout (clamped to remaining ctx).
func (pf *proxyForwarder) dialBinding(ctx context.Context, entry proxy.Entry, local *net.TCPAddr) (net.Conn, string, error) {
	var lastErr error
	names := pf.orderedNames(entry)
	for round := 1; round <= proxyDialMaxRounds; round++ {
		for _, name := range names {
			p, err := pf.store.GetByName(ctx, name)
			if err != nil {
				lastErr = err
				continue
			}
			dialer, err := proxy.NewDialer(p)
			if err != nil {
				lastErr = err
				continue
			}
			actx, cancel := context.WithTimeout(ctx, proxyDialAttemptTimeout)
			c, err := dialer.Dial(actx, entry.Hostname, local.IP, local.Port)
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
			return c, name, nil
		}
		if ctx.Err() != nil {
			break
		}
		if round < proxyDialMaxRounds {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(proxyDialBackoffBase * time.Duration(round)):
			}
		}
	}
	return nil, "", lastErr
}

// handleUDP services one UDP flow netstack accepted on the utun: open a
// SOCKS5 UDP association for the rule's proxy and relay datagrams until a
// side errors or the flow goes idle. The drop is gated on UDP capability,
// not the port — associateUDP only succeeds for a SOCKS5 upstream (plain
// proxy or xray entry), so HTTP-only bindings yield no session and fall
// back to TCP. QUIC (UDP/443) is relayed too rather than dropped up front,
// which the old blanket drop broke for QUIC-first backends like signaler-pa.
func (pf *proxyForwarder) handleUDP(conn net.Conn, local, remote *net.UDPAddr) {
	defer conn.Close()

	entry, ok := pf.table.Lookup(local.IP)
	if !ok {
		entry, ok = pf.lookupIPRule(local.IP)
		if !ok {
			pf.logger.Printf("proxytun/udp: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
			return
		}
	}

	// Relay to the ORIGINAL hostname, not local.IP — for a fake-IP routed
	// name local.IP is a 198.18.x.x handle that only means something on this
	// machine, so a datagram addressed to it dies at the proxy's egress.
	target := entry.Hostname
	if target == "" {
		target = local.IP.String()
	}

	dctx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
	session, used, err := pf.associateUDP(dctx, entry, nil)
	cancel()
	if session == nil {
		pf.logger.Printf("proxytun/udp: %s:%d — no usable proxy: %v", target, local.Port, err)
		return
	}

	// The session can be replaced mid-flow when it turns out to be dead (see
	// the watchdog below), so the pumps read it from cur instead of closing
	// over one pointer. Publish order on a swap is always: store the new
	// session, THEN close the old one — that's what lets a pump tell "we were
	// swapped, keep going" apart from "this flow is over".
	var cur atomic.Pointer[proxy.UDPSession]
	cur.Store(session)

	// Snapshot the watchdog tuning once per flow rather than re-reading the
	// globals on every tick: one flow then runs on one consistent timeline.
	noReplyAfter, monitorTick := proxyUDPNoReplyTimeout, proxyUDPMonitorTick

	stopKeepalive := pf.keepRouteAlive(local.IP)
	defer stopKeepalive()

	startedAt := time.Now()
	pf.logger.Printf("proxytun/udp: %s:%d via proxy %q", target, local.Port, used)

	// usedName is the proxy currently carrying the flow (byte accounting
	// follows it); chain is the human-readable history for the closing log.
	var nameMu sync.Mutex
	usedName, chain := used, used
	currentName := func() string { nameMu.Lock(); defer nameMu.Unlock(); return usedName }
	nameChain := func() string { nameMu.Lock(); defer nameMu.Unlock(); return chain }

	// Single idempotent teardown. Closing both ends unblocks every pump's
	// blocking Read so they error out and exit — no per-read deadlines.
	var closed atomic.Bool
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			closed.Store(true)
			_ = conn.Close()
			_ = cur.Load().Close()
		})
	}
	defer shutdown()

	// Idle is measured across BOTH directions: either pump bumps lastActive
	// on a datagram, so a flow that's busy one-way stays up. A monitor tears
	// it down only after proxyUDPIdleTimeout of total silence.
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())

	// Live byte counters for usage stats. sent = client→proxy datagrams,
	// recv = proxy→client. Flushed as deltas by the idle monitor below and
	// once more after teardown, mirroring the TCP splice accounting.
	var sentBytes, recvBytes atomic.Int64
	var flushedSent, flushedRecv int64
	var flushMu sync.Mutex
	flush := func() {
		flushMu.Lock()
		defer flushMu.Unlock()
		s, r := sentBytes.Load(), recvBytes.Load()
		dS, dR := s-flushedSent, r-flushedRecv
		flushedSent, flushedRecv = s, r
		if dS != 0 || dR != 0 {
			pf.recordTraffic(entry.Hostname, currentName(), dS, dR)
		}
	}

	// watchCtrl tears the flow down when a session's SOCKS5 control conn
	// closes (the proxy dropped the association). One per session: after a
	// swap the retired session's watcher must NOT kill the live flow, which
	// it detects by no longer being the current session.
	watchCtrl := func(s *proxy.UDPSession) {
		b := make([]byte, 1)
		_, _ = s.CtrlConn().Read(b)
		if closed.Load() || cur.Load() != s {
			return
		}
		shutdown()
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// client → relay
	go func() {
		defer wg.Done()
		defer shutdown()
		buf := make([]byte, 64*1024)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				lastActive.Store(time.Now().UnixNano())
				s := cur.Load()
				if werr := s.WriteTo(buf[:n], target, local.Port); werr != nil {
					// A swap can close this session out from under us. Resend
					// through the replacement instead of dropping the flow.
					ns := cur.Load()
					if closed.Load() || ns == s {
						return
					}
					if werr := ns.WriteTo(buf[:n], target, local.Port); werr != nil {
						return
					}
				}
				sentBytes.Add(int64(n))
			}
			if rerr != nil {
				return
			}
		}
	}()

	// relay → client
	go func() {
		defer wg.Done()
		defer shutdown()
		buf := make([]byte, 64*1024)
		for {
			s := cur.Load()
			n, rerr := s.Read(buf)
			if n > 0 {
				lastActive.Store(time.Now().UnixNano())
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
				recvBytes.Add(int64(n))
			}
			if rerr != nil {
				// Same distinction as the writer: a swapped-out session errors
				// by design, and the flow continues on its replacement.
				if closed.Load() || cur.Load() == s {
					return
				}
				continue
			}
		}
	}()

	go watchCtrl(session)

	// Flow watchdog. Two independent teardown conditions:
	//
	//   - no-reply: we've sent, nothing has ever come back, and the grace
	//     period is up. The association is dead; cut it so the client's next
	//     retransmit gets a fresh one.
	//   - idle: silence in BOTH directions for the full idle timeout. UDP has
	//     no FIN, so this is what reclaims a finished flow.
	//
	// Byte deltas are flushed on a slower cadence than the tick so long-lived
	// flows (QUIC) register on the usage dashboard before they end.
	var noReply atomic.Bool
	// noReplySince is the clock the no-reply rule measures against. It restarts
	// on every re-association so a replacement gets its own full grace period.
	var noReplySince atomic.Int64
	noReplySince.Store(startedAt.UnixNano())

	// reassociate moves the flow onto another association — a different proxy
	// from the binding when there is one, otherwise a fresh association on the
	// same proxy (a dead association is per-association, not per-proxy: the
	// same upstream serves a sibling flow fine seconds later). Only ever
	// called while nothing has been received, so there's no QUIC connection
	// state to lose — the client's retransmits simply take the new path.
	tried := map[string]struct{}{used: {}}
	swaps := 0
	reassociate := func() bool {
		if swaps >= proxyUDPMaxReassoc {
			return false
		}
		old := cur.Load()
		prev := currentName()
		flush() // bill what we sent to the association that swallowed it

		actx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
		ns, nname, aerr := pf.associateUDP(actx, entry, tried)
		cancel()
		if ns == nil {
			pf.logger.Printf("proxytun/udp: %s:%d — re-associate failed: %v", target, local.Port, aerr)
			return false
		}

		swaps++
		tried[nname] = struct{}{}
		nameMu.Lock()
		usedName = nname
		chain += " → " + nname
		nameMu.Unlock()

		cur.Store(ns) // publish first…
		_ = old.Close()
		go watchCtrl(ns)
		noReplySince.Store(time.Now().UnixNano())
		pf.logger.Printf("proxytun/udp: %s:%d silent via %q after %s — re-associated via %q",
			target, local.Port, prev, noReplyAfter, nname)
		return true
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(monitorTick)
		defer t.Stop()
		flushEvery := int(proxyUDPIdleTimeout / 2 / monitorTick)
		if flushEvery < 1 {
			flushEvery = 1
		}
		for ticks := 1; ; ticks++ {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if ticks%flushEvery == 0 {
				flush()
			}
			if recvBytes.Load() == 0 && sentBytes.Load() > 0 &&
				time.Since(time.Unix(0, noReplySince.Load())) >= noReplyAfter {
				if reassociate() {
					continue
				}
				noReply.Store(true)
				shutdown()
				return
			}
			if time.Since(time.Unix(0, lastActive.Load())) >= proxyUDPIdleTimeout {
				shutdown()
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	flush() // final delta after teardown

	// Per-flow summary. "Flow opened" says nothing about whether the relay
	// actually carried anything — a black-holed QUIC flow opens fine, sends,
	// and never hears back. recv=0 after a real send is exactly that failure
	// signature, so it's called out rather than left to be eyeballed.
	sent, recv := sentBytes.Load(), recvBytes.Load()
	dur := time.Since(startedAt).Round(time.Millisecond)
	if sent > 0 && recv == 0 {
		why := "relay black hole?"
		if noReply.Load() {
			why = "every association stayed silent; client falls back to TCP"
		}
		pf.logger.Printf("proxytun/udp: %s:%d closed via %q — sent %s, RECEIVED NOTHING in %s (%s)",
			target, local.Port, nameChain(), humanBytes(sent), dur, why)
		return
	}
	pf.logger.Printf("proxytun/udp: %s:%d closed via %q — sent %s, recv %s in %s",
		target, local.Port, nameChain(), humanBytes(sent), humanBytes(recv), dur)
}

// humanBytes renders a byte count for log lines. Binary units — these are
// transfer sizes, not disk marketing.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// associateUDP walks the rule's proxy binding and returns the first
// SOCKS5 proxy for which a UDP ASSOCIATE succeeds. HTTP proxies are
// skipped (no UDP support). Returns (nil, "", lastErr) if none work.
//
// Names in tried are pushed to the back rather than excluded: they've already
// produced a dead association for this flow, so prefer anything else — but a
// second association on the same proxy usually works, so it beats no relay at
// all when the binding has a single name.
func (pf *proxyForwarder) associateUDP(ctx context.Context, entry proxy.Entry, tried map[string]struct{}) (*proxy.UDPSession, string, error) {
	var lastErr error
	names := pf.orderedNames(entry)
	if len(tried) > 0 {
		fresh := make([]string, 0, len(names))
		stale := make([]string, 0, len(names))
		for _, n := range names {
			if _, ok := tried[n]; ok {
				stale = append(stale, n)
			} else {
				fresh = append(fresh, n)
			}
		}
		names = append(fresh, stale...)
	}
	for round := 1; round <= proxyDialMaxRounds; round++ {
		for _, name := range names {
			p, err := pf.store.GetByName(ctx, name)
			if err != nil {
				lastErr = err
				continue
			}
			if p.Protocol != proxy.ProtocolSOCKS5 {
				lastErr = fmt.Errorf("proxy %q is %s; UDP requires socks5", name, p.Protocol)
				continue
			}
			actx, cancel := context.WithTimeout(ctx, proxyDialAttemptTimeout)
			sess, err := proxy.DialUDPAssociate(actx, p)
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
			return sess, name, nil
		}
		if ctx.Err() != nil {
			break
		}
		if round < proxyDialMaxRounds {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(proxyDialBackoffBase * time.Duration(round)):
			}
		}
	}
	return nil, "", lastErr
}

// startProxyTunnel opens the daemon-owned utun and builds the netstack
// tunnel that terminates proxy-routed TCP connections. The caller is
// responsible for running tunnel.Start(ctx) (it blocks) and Stop()ping
// on shutdown.
//
// Failure is NON-fatal: opening a utun needs root, which the dev
// `make run-daemon` flow lacks. On failure we return (nil, "") and the
// daemon runs without proxy support — dnsproxy treats proxy: rules as
// unsupported (logged "block-proxy-unsupported") whenever ProxyTun is
// empty.
func startProxyTunnel(store *proxy.Store, table *proxy.Table, router *routing.Manager, decider ipRouteDecider, latency *netprobe.LatencyTracker, traffic *trafficAggregator, logger *log.Logger) (*proxytun.Tunnel, string) {
	tun, err := proxytun.Open(proxyUTUNAddr, 1500)
	if err != nil {
		logger.Printf("em-walld: proxy utun open failed (proxy routing disabled): %v", err)
		return nil, ""
	}
	fwd := &proxyForwarder{store: store, table: table, router: router, decider: decider, latency: latency, traffic: traffic, logger: logger}
	tunnel, err := proxytun.NewTunnel(tun, 1500, fwd.handle, fwd.handleUDP, logger)
	if err != nil {
		logger.Printf("em-walld: proxy tunnel init failed (proxy routing disabled): %v", err)
		_ = tun.Close()
		return nil, ""
	}
	logger.Printf("em-walld: proxy tunnel up on %s (addr %s)", tunnel.IfaceName(), proxyUTUNAddr)
	return tunnel, tunnel.IfaceName()
}
