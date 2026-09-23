package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	proxyUDPNoReplyTimeout = 3 * time.Second
	proxyUDPMonitorTick    = time.Second
)

// proxyUDPMaxReassoc caps how many times one flow may be moved to another
// association before we give up and let it die.
//
// Measured on a live install: of ~3000 flows that went silent and were
// re-associated, ~1% ever recovered. Every extra swap is therefore ~3s of
// the client sitting on a handshake, bought for a 1-in-100 chance. One
// swap clears the genuinely per-association case; past that the honest
// move is to die fast and let udpHealth refuse the next attempt outright
// so the client gets ICMP instead of silence.
const proxyUDPMaxReassoc = 1

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

// Opening-exchange tuning for path verification (see dialBinding).
//
// proxyClientHelloWait bounds how long we wait for the client to say
// something before giving up on verifying the path. Clients that speak
// first do so immediately (the ClientHello is already queued when the SYN
// completes); waiting longer only delays server-speaks-first protocols.
//
// proxyFirstByteTimeout is how long a TLS server may stay silent before we
// call the path dead. It must cover a slow exit's round trip to a distant
// origin, but stay well inside the browser's own patience.
//
// Both are vars rather than consts so tests can compress the timeline.
var (
	proxyClientHelloWait  = time.Second
	proxyFirstByteTimeout = 8 * time.Second
)

// Hedged-race tuning for verified dials (see raceRound).
//
// proxyHedgeDelay is how long one member may stay silent before the next
// starts beside it. Measured through the live members, a healthy path
// returns the first TLS byte in ~0.3–0.9s, so this is past a normal reply
// but far below the first-byte timeout. It is also the head start the
// sticky incumbent gets over a challenger.
//
// proxyHedgeMaxInFlight bounds how many members one connection has open at
// once. Two is enough to route around a stalled path; more would multiply
// load on an uplink that is already struggling when hedging kicks in.
//
// With racing, proxyFirstByteTimeout no longer gates failover — a sibling
// starts after proxyHedgeDelay regardless — so it can be generous: it is
// how long a slow-but-working path is given to win, and a TLS reply on a
// lossy link can need several retransmission timeouts (1s+2s+4s).
var proxyHedgeDelay = 1200 * time.Millisecond

const proxyHedgeMaxInFlight = 2

const proxyOpeningReadSize = 16 * 1024 // one TLS record; ClientHellos fit

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
	sticky  *stickyBindings    // remembers which upstream a destination is on; nil disables
	traffic *trafficAggregator // nil disables byte accounting
	health  *udpHealth         // remembers dead UDP relay paths; nil disables
	breaker *tcpHealth         // remembers dead TCP paths; nil disables
	gate    *dialGate          // bounds concurrent dials; nil disables
	sampler *logSampler        // rate-limits the per-connection log line; nil disables
	witness *uplinkWitness     // proves the uplink was up before blaming a silent upstream; nil = always blame
	stats   *connStats         // connection health measurements; nil disables
	routes  *routeKeys         // which upstreams share a way in; nil = all independent
	logger  *log.Logger
}

// logVia writes the per-connection "via proxy" line through the sampler,
// so a destination being retried in a loop can't turn the log into the
// bottleneck. prefix distinguishes the TCP and UDP call sites.
func (pf *proxyForwarder) logVia(prefix, target string, port int, used string) {
	ok, suppressed := pf.sampler.allow(fmt.Sprintf("%s|%s|%d|%s", prefix, target, port, used))
	if !ok {
		return
	}
	if suppressed > 0 {
		pf.logger.Printf("%s: %s:%d via proxy %q (+%d more suppressed)", prefix, target, port, used, suppressed)
		return
	}
	pf.logger.Printf("%s: %s:%d via proxy %q", prefix, target, port, used)
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

// stickyKey identifies the destination a binding decision is remembered
// under. The hostname is the right granularity — it is what an origin ties a
// session to. An IP/CIDR-matched rule has no DNS name, and lookupIPRule
// already puts the destination address there, so that case keys by address
// without needing a branch here.
func stickyKey(entry proxy.Entry) string { return entry.Hostname }

// resetHealth drops every destination verdict after a network change.
// Sticky bindings are kept on purpose: which exit a site is on is still
// worth keeping stable, and a binding whose member fails is dropped by the
// normal path anyway.
func (pf *proxyForwarder) resetHealth() {
	if pf == nil {
		return
	}
	pf.breaker.reset()
	pf.health.reset()
}

// incumbent is the upstream a connection should prefer: the one this exact
// host last used, else the one its site last used. A site like YouTube
// spreads over dozens of hosts (rr1---sn-…, rr5---sn-…); without the site
// fallback every new host re-ranks from scratch and can land on a member
// that doesn't work for that site, while a sibling host already found one
// that does. It is only a starting preference — RankFrom still demotes an
// unhealthy incumbent and still lets a much faster member take over.
func (pf *proxyForwarder) incumbent(entry proxy.Entry) string {
	if n := pf.sticky.Get(stickyKey(entry)); n != "" {
		return n
	}
	return pf.sticky.Get(siteKey(entry.Hostname))
}

// siteKey groups a hostname with its siblings under the registrable
// domain: "rr1---sn-abc.googlevideo.com" → "site:googlevideo.com". A
// heuristic, not a public-suffix lookup: the last two labels, or three
// under a two-letter country code with a generic second level
// ("x.co.uk"). IP literals and bare two-label names have no siblings to
// learn from and return "".
func siteKey(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return ""
	}
	n := 2
	if tld, sld := labels[len(labels)-1], labels[len(labels)-2]; len(tld) == 2 {
		switch sld {
		case "co", "com", "net", "org", "gov", "edu", "ac":
			n = 3
		}
	}
	if len(labels) <= n {
		return ""
	}
	return "site:" + strings.Join(labels[len(labels)-n:], ".")
}

// orderedNames returns the binding's proxy names best-first when a latency
// tracker is wired, else the binding's original order. The destination's
// current upstream (if any) is passed as the ranking incumbent, so it is
// only displaced by an upstream that is healthier or meaningfully faster —
// without that, every probe round reshuffles the list and the next
// connection to the same site leaves from a different exit IP.
// Single-name bindings pass through untouched.
func (pf *proxyForwarder) orderedNames(entry proxy.Entry) []string {
	if pf.latency == nil {
		return entry.ProxyNames
	}
	return pf.latency.RankFrom(entry.ProxyNames, pf.incumbent(entry))
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
	start := time.Now()

	entry, ok := pf.table.Lookup(local.IP)
	if !ok {
		// No DNS-time mapping. Either an IP/CIDR rule routed this real
		// dest IP to our utun (resolve it via the decision engine), or
		// it's a stale route lingering after a table sweep — drop then.
		entry, ok = pf.lookupIPRule(local.IP)
		if !ok {
			pf.logger.Printf("proxytun: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
			pf.stats.failed(causeNoMapping)
			return
		}
	}

	target := entry.Hostname
	if target == "" {
		target = local.IP.String()
	}

	healthKey := tcpHealthKey(target, local.Port)

	// A destination we already know is dead through this binding is closed
	// at once rather than walked through the full fallback list again. The
	// client is going to fail either way; the difference is whether it
	// costs us fifteen upstream dials and twenty seconds each time it
	// retries. admit still lets one connection per second through, so the
	// moment the path comes back this stops happening.
	if !pf.breaker.admit(healthKey) {
		pf.stats.failed(causePaused)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
	defer cancel()

	// Bound concurrent dials, and release the slot the instant dialing is
	// over — the splice below may run for hours and holds nothing this
	// ceiling is protecting.
	release, got := pf.gate.acquire(ctx)
	if !got {
		pf.stats.failed(causeDialCeiling)
		if ok, n := pf.sampler.allow("gate|" + healthKey); ok {
			pf.logger.Printf("proxytun: %s:%d — dial ceiling (%d) saturated; dropping (+%d more suppressed)", target, local.Port, proxyMaxConcurrentDials, n)
		}
		return
	}
	upstream, used, sent, lastErr := pf.dialBinding(ctx, entry, local, conn)
	release()
	if upstream == nil && errors.Is(lastErr, errDestinationRefused) {
		// A verdict on the destination, not a flaky path: pause it now, so
		// the client's retries fail in ~0s and it moves on (a video player
		// falls back to another host) instead of waiting out another walk.
		pf.stats.failed(causeDestRefused)
		d := pf.breaker.condemn(healthKey)
		if ok, n := pf.sampler.allow("refused|" + healthKey); ok {
			pf.logger.Printf("proxytun: %s:%d — every upstream refused it (%v); pausing this destination for %s (+%d more suppressed)",
				target, local.Port, lastErr, d, n)
		}
		return
	}
	if upstream == nil {
		pf.stats.failed(causeNoUpstream)
		// Every member of the binding failed: a total failure, and the
		// one dial outcome strong enough to count against the destination.
		if d := pf.breaker.strike(healthKey); d > 0 {
			pf.logger.Printf("proxytun: %s:%d — no upstream carried it: %v; pausing this destination for %s",
				target, local.Port, lastErr, d)
			return
		}
		// Not the transition, so sample it: below the threshold this line
		// still fires once per client retry, and a retry loop is exactly
		// the situation where it must not become the bottleneck.
		if ok, n := pf.sampler.allow("faildial|" + healthKey); ok {
			pf.logger.Printf("proxytun: %s:%d — no upstream carried it: %v (+%d more suppressed)",
				target, local.Port, lastErr, n)
		}
		return
	}
	defer upstream.Close()
	pf.stats.established(used, time.Since(start))

	// Keep the fake-IP route + mapping alive for as long as we're splicing,
	// so the TTL sweep can't cut this connection out from under us.
	stop := pf.keepRouteAlive(local.IP)
	defer stop()

	pf.logVia("proxytun", target, local.Port, used)
	// The client's opening bytes were consumed by dialBinding and forwarded
	// there, so bill them here — SpliceCounted only sees what follows.
	if sent > 0 {
		pf.recordTraffic(entry.Hostname, used, sent, 0)
	}
	// conn→upstream is bytes the client sent; upstream→conn is bytes it
	// received. SpliceCounted reports deltas live so long-lived streams
	// register on the usage dashboard before they close.
	atob, btoa, _ := proxy.SpliceCounted(conn, upstream, proxyTrafficFlushInterval, func(a, b int64) {
		pf.recordTraffic(entry.Hostname, used, a, b)
	})

	// A connection that sent bytes and heard nothing back is evidence the
	// chosen upstream cannot carry this destination — the same signal
	// udpHealth acts on, and the only one available for a TCP path whose
	// SOCKS handshake always succeeds. One is not proof (clients abort
	// connections all the time), so it is a strike, not a verdict:
	// netprobe demotes a name only on consecutive failures, and tcpHealth
	// only pauses the destination after tcpStrikeThreshold of them.
	//
	// A single byte back is proof of the opposite, and clears the record
	// outright — this is what keeps a destination from being paused for a
	// path that already recovered.
	switch {
	case btoa > 0:
		pf.breaker.success(healthKey)
		pf.noteUpstreamSuccess(used)
	case atob+sent > 0:
		pf.stats.noData(used)
		// Blame the upstream only if the uplink demonstrably worked: some
		// other upstream carried data just now (see uplinkWitness).
		if pf.witness.otherAlive(used) {
			pf.noteUpstreamFailure(entry, used)
		}
		if d := pf.breaker.strike(healthKey); d > 0 {
			pf.logger.Printf("proxytun: %s:%d — carried no data through %q; pausing this destination for %s",
				target, local.Port, used, d)
		}
	}
}

// noteUpstreamFailure records that name failed to carry entry's traffic:
// the destination stops preferring it, and the latency tracker takes a
// strike against it so ranking demotes it once the failure repeats.
func (pf *proxyForwarder) noteUpstreamFailure(entry proxy.Entry, name string) {
	pf.stats.blamed(name)
	pf.sticky.Drop(stickyKey(entry), name)
	pf.sticky.Drop(siteKey(entry.Hostname), name) // don't hand a failing member to siblings
	if pf.latency != nil {
		pf.latency.Fail(name)
	}
}

// noteUpstreamSuccess records that name actually carried data. Probes run
// every 30s; connections run continuously, so without this the breaker's
// window between rounds would be built out of failures alone — a name
// doing real work would look identical to one sitting idle, and a single
// bad destination could demote an upstream the rest of the system is
// using happily.
func (pf *proxyForwarder) noteUpstreamSuccess(name string) {
	pf.witness.saw(name)
	if pf.latency != nil {
		pf.latency.Succeed(name)
	}
}

// dialBinding finds a member of the rule's proxy binding that carries the
// connection — best-ranked first, raced (raceRound) when the path can be
// verified, walked in order (serialRound) when it can't — and retries the
// whole binding with backoff on transient failure. Each dial is capped by
// proxyDialAttemptTimeout (clamped to remaining ctx).
// Returns the upstream, the name carrying it, and how many client bytes were
// already forwarded onto it (the caller bills those; the splice never sees
// them).
//
// A successful dial is NOT evidence the upstream works. Every xray entry is
// a loopback SOCKS5 inbound, and xray answers CONNECT with success the
// moment it parses the request — before it has dialed the far side, or even
// learned whether it can. Measured on a live install: a CONNECT to a
// blackholed address returns rep=0 in 0.000s. So for xray bindings the dial
// error is a constant "fine", the fallback list below never advances, and a
// dead node silently swallows every connection routed to it. That is the
// difference the user sees between "my set failed over" and "the internet
// stopped".
//
// So when the client opens with a TLS ClientHello we verify the path
// instead of trusting it: forward the hello and require a byte back within
// proxyFirstByteTimeout. TLS servers answer within one RTT by definition,
// so silence means the path is dead, and another member is raced against
// it with the hello replayed — safe, because the client has received
// nothing and a ClientHello carries no side effects. Anything that is not a ClientHello (plaintext
// HTTP, server-speaks-first protocols, long-poll shapes where silence is
// legitimate) is forwarded unverified, exactly as before.
func (pf *proxyForwarder) dialBinding(ctx context.Context, entry proxy.Entry, local *net.TCPAddr, client net.Conn) (net.Conn, string, int64, error) {
	names := pf.orderedNames(entry)
	key := stickyKey(entry)

	// Read the client's opening bytes once, up front: they are both what we
	// replay onto each candidate and how we tell a verifiable connection
	// from an unverifiable one. Only on ports where the client is known to
	// speak first — waiting on an SSH or SMTP client that is itself waiting
	// for a banner would just add the timeout to every such connection.
	var hello []byte
	if clientSpeaksFirst(local.Port) {
		var err error
		hello, err = readOpening(client, proxyClientHelloWait)
		if err != nil && len(hello) == 0 {
			return nil, "", 0, fmt.Errorf("client sent nothing: %w", err)
		}
	}
	verify := looksLikeTLSClientHello(hello)

	// Members that failed this connection. They are only blamed once
	// another member has carried it: when EVERY member fails at once, the
	// common factor is the user's own uplink or the destination, not the
	// members — striking all of them just demotes the whole set in lockstep
	// (observed: every member of every set flapping together through a
	// local outage), which scrambles ranking and drops sticky bindings for
	// nothing. The destination-level brake (tcpHealth) covers that case.
	var failed []string
	var lastErr error
	for round := 1; round <= proxyDialMaxRounds; round++ {
		var up net.Conn
		var used string
		if verify {
			up, used, failed, lastErr = pf.raceRound(ctx, entry, local, names, hello, failed)
		} else {
			up, used, failed, lastErr = pf.serialRound(ctx, entry, local, names, hello, failed)
		}
		if up != nil {
			for _, n := range failed {
				if n != used {
					pf.noteUpstreamFailure(entry, n)
				}
			}
			if verify {
				pf.witness.saw(used) // it answered the hello: data came back
			}
			pf.sticky.Set(key, used)
			pf.sticky.Set(siteKey(entry.Hostname), used)
			return up, used, int64(len(hello)), nil
		}
		if ctx.Err() != nil || errors.Is(lastErr, errDestinationRefused) {
			break
		}
		if round < proxyDialMaxRounds {
			select {
			case <-ctx.Done():
				return nil, "", 0, ctx.Err()
			case <-time.After(proxyDialBackoffBase * time.Duration(round)):
			}
		}
	}
	return nil, "", 0, lastErr
}

// errUpstreamRefused marks one member whose exit actively closed the
// connection before answering the ClientHello — as opposed to staying
// silent. xray's SOCKS inbound accepts every CONNECT, so this is how an
// exit that could not reach (or resolve) the destination shows up.
var errUpstreamRefused = errors.New("upstream closed the connection before replying")

// errDestinationRefused is a whole race round of errUpstreamRefused: every
// member reached its exit and every exit turned the destination away.
var errDestinationRefused = errors.New("every upstream refused the destination")

// upstreamClosed reports a read error that means the far side closed or
// reset the connection, not a timeout.
func upstreamClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF)
}

// appendFailed records name in failed once.
func appendFailed(failed []string, name string) []string {
	for _, n := range failed {
		if n == name {
			return failed
		}
	}
	return append(failed, name)
}

// serialRound is one pass over the binding for a connection that can't be
// verified: the first member whose dial succeeds carries it. There is no
// point racing here — without a reply to wait for, a dial to a loopback
// SOCKS inbound succeeds (or fails) at once.
func (pf *proxyForwarder) serialRound(ctx context.Context, entry proxy.Entry, local *net.TCPAddr, names []string, hello []byte, failed []string) (net.Conn, string, []string, error) {
	var lastErr error
	for _, name := range names {
		c, err := pf.dialMember(ctx, entry, local, name)
		if err == nil {
			// Unverified: forward whatever the client already said, but
			// don't wait for a reply (wait 0).
			var up net.Conn
			if up, err = pf.primeUpstream(c, hello, 0); err == nil {
				return up, name, failed, nil
			}
			_ = c.Close()
			err = fmt.Errorf("proxy %q: %w", name, err)
		}
		lastErr = err
		failed = appendFailed(failed, name)
	}
	return nil, "", failed, lastErr
}

// dialMember opens one member's tunnel to the destination.
func (pf *proxyForwarder) dialMember(ctx context.Context, entry proxy.Entry, local *net.TCPAddr, name string) (net.Conn, error) {
	p, err := pf.store.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	dialer, err := proxy.NewDialer(p)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithTimeout(ctx, proxyDialAttemptTimeout)
	defer cancel()
	return dialer.Dial(actx, entry.Hostname, local.IP, local.Port)
}

// dialResult is one raced attempt's outcome.
type dialResult struct {
	name string
	conn net.Conn
	err  error
}

// raceRound is one pass over the binding for a TLS connection, run as a
// hedged race rather than a walk. The best-ranked member starts alone; if
// it hasn't answered within proxyHedgeDelay the next one starts beside it
// (and a failure starts the next one at once), at most
// proxyHedgeMaxInFlight at a time, each replaying the same ClientHello.
// The first member to answer carries the connection; the rest are closed.
//
// A serial walk made every silent member cost the full first-byte timeout
// before the next was even tried, so a five-member set could spend the
// whole connect deadline on four stalls and never reach the fifth — and on
// a lossy link it abandoned paths that were merely slow. Here a slow path
// keeps running while a sibling is tried, so whichever is actually faster
// wins, and the incumbent still gets proxyHedgeDelay of head start, which
// keeps a destination on its sticky exit whenever that exit is healthy.
//
// Replaying the hello on two paths is safe for the same reason replaying
// it serially is: a ClientHello has no side effects, and the loser is
// closed before the client has seen a byte from it.
func (pf *proxyForwarder) raceRound(ctx context.Context, entry proxy.Entry, local *net.TCPAddr, names []string, hello []byte, failed []string) (net.Conn, string, []string, error) {
	if len(names) == 0 {
		return nil, "", failed, fmt.Errorf("empty binding")
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dialResult, len(names))
	wait := proxyFirstByteTimeout // read once: attempts may outlive this call
	next, inFlight := 0, 0        // next = how many members have been launched
	order := diverseOrder(names, pf.routeOf)
	launch := func() {
		if next > 0 {
			pf.stats.hedged() // an extra attempt beyond the first
		}
		name := order[next]
		next++
		inFlight++
		go func() {
			c, err := pf.attemptVerified(rctx, entry, local, name, hello, wait)
			results <- dialResult{name: name, conn: c, err: err}
		}()
	}
	// Losers still in flight when the race is decided are closed as they
	// land; results is buffered for every launch, so nothing blocks.
	drain := func(n int) {
		go func() {
			for i := 0; i < n; i++ {
				if r := <-results; r.conn != nil {
					_ = r.conn.Close()
				}
			}
		}()
	}

	launch()
	hedge := time.NewTimer(proxyHedgeDelay)
	defer hedge.Stop()
	var lastErr error
	refused := 0
	for inFlight > 0 {
		select {
		case r := <-results:
			inFlight--
			if r.err == nil {
				cancel() // stop the losers' waits; drain closes what they return
				drain(inFlight)
				return r.conn, r.name, failed, nil
			}
			lastErr = r.err
			if errors.Is(r.err, errUpstreamRefused) {
				refused++
			}
			failed = appendFailed(failed, r.name)
			if next < len(names) {
				launch()
				hedge.Reset(proxyHedgeDelay)
			}
		case <-hedge.C:
			if next < len(names) && inFlight < proxyHedgeMaxInFlight {
				launch()
			}
			hedge.Reset(proxyHedgeDelay)
		case <-ctx.Done():
			cancel()
			drain(inFlight)
			return nil, "", failed, ctx.Err()
		}
	}
	if refused == len(names) {
		// Every member reached its exit and was turned away: the
		// destination itself is unreachable (typically a name that doesn't
		// resolve at the exit — FakeIP answered it locally). Another round
		// would only make the client wait longer for the same answer.
		return nil, "", failed, fmt.Errorf("%w: %v", errDestinationRefused, lastErr)
	}
	return nil, "", failed, lastErr
}

// diverseOrder reorders a ranked binding so that consecutive launches
// cover different routes (see routekeys.go) before any route is tried
// twice: the best member first, then the best member on a route not yet
// tried, and so on, falling back to rank order once every route has been
// used. Within a route, rank order is kept, so the sticky incumbent still
// goes first and every member is still reached.
func diverseOrder(ranked []string, routeOf func(string) string) []string {
	if len(ranked) < 3 {
		return ranked // two members: nothing to reorder
	}
	out := make([]string, 0, len(ranked))
	used := make([]bool, len(ranked))
	seen := map[string]bool{}
	for len(out) < len(ranked) {
		picked := -1
		for i, n := range ranked {
			if !used[i] && !seen[routeOf(n)] {
				picked = i
				break
			}
		}
		if picked < 0 { // every route tried once: start the next pass
			seen = map[string]bool{}
			continue
		}
		used[picked] = true
		seen[routeOf(ranked[picked])] = true
		out = append(out, ranked[picked])
	}
	return out
}

// attemptVerified dials one member and proves the path with the client's
// hello (see primeUpstream). Cancelling ctx aborts the wait for the reply,
// so a race that has already been won doesn't hold its losers open.
func (pf *proxyForwarder) attemptVerified(ctx context.Context, entry proxy.Entry, local *net.TCPAddr, name string, hello []byte, wait time.Duration) (net.Conn, error) {
	c, err := pf.dialMember(ctx, entry, local, name)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
	up, err := pf.primeUpstream(c, hello, wait)
	if !stop() || err != nil {
		_ = c.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("proxy %q: %w", name, err)
	}
	return up, nil
}

// primeUpstream forwards the client's opening bytes onto a freshly dialed
// upstream and, when wait > 0, waits up to wait for the first byte of the reply.
// The bytes it reads ahead are handed back in front of the connection so
// the splice still delivers them. Any error means this upstream is not
// carrying the connection — the caller closes it and tries the next.
func (pf *proxyForwarder) primeUpstream(c net.Conn, hello []byte, wait time.Duration) (net.Conn, error) {
	if len(hello) == 0 {
		return c, nil
	}
	if _, err := c.Write(hello); err != nil {
		return nil, fmt.Errorf("forward opening bytes: %w", err)
	}
	if wait <= 0 {
		return c, nil
	}
	buf := make([]byte, 1024)
	if err := c.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return c, nil // connection can't be verified; don't reject it for that
	}
	n, err := c.Read(buf)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil && n == 0 {
		if upstreamClosed(err) {
			return nil, fmt.Errorf("%w: %v", errUpstreamRefused, err)
		}
		return nil, fmt.Errorf("no reply within %s: %w", wait, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("no reply within %s", wait)
	}
	return &prefixConn{Conn: c, prefix: buf[:n]}, nil
}

// readOpening reads whatever the client sends first, up to wait. A client
// that says nothing (server-speaks-first protocols) yields no bytes and no
// fatal error — the connection is then forwarded unverified.
func readOpening(client net.Conn, wait time.Duration) ([]byte, error) {
	buf := make([]byte, proxyOpeningReadSize)
	if err := client.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, err
	}
	n, err := client.Read(buf)
	_ = client.SetReadDeadline(time.Time{})
	if n > 0 {
		return buf[:n], nil
	}
	return nil, err
}

// clientSpeaksFirst reports whether port is one where the client opens the
// conversation, making it safe to wait briefly for its first bytes. TLS
// ports only: that is where the traffic worth verifying is, and it keeps
// every server-speaks-first protocol on the untouched path.
func clientSpeaksFirst(port int) bool {
	switch port {
	case 443, 853, 993, 995, 8443:
		return true
	}
	return false
}

// looksLikeTLSClientHello reports whether b opens a TLS handshake: record
// type 22 (handshake) followed by a 3.x legacy version. That is the one
// shape where a silent server is unambiguously a broken path — the peer
// owes a ServerHello within an RTT — and the one where replaying the
// opening bytes elsewhere is free of side effects.
func looksLikeTLSClientHello(b []byte) bool {
	return len(b) >= 3 && b[0] == 0x16 && b[1] == 0x03
}

// prefixConn serves already-read bytes before falling through to the
// connection itself.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// handleUDP services one UDP flow netstack accepted on the utun: open a
// SOCKS5 UDP association for the rule's proxy and relay datagrams until a
// side errors or the flow goes idle. The drop is gated on UDP capability,
// not the port — associateUDP only succeeds for a SOCKS5 upstream (plain
// proxy or xray entry), so HTTP-only bindings yield no session and fall
// back to TCP. QUIC (UDP/443) is relayed too rather than dropped up front,
// which the old blanket drop broke for QUIC-first backends like signaler-pa.
// udpDestination resolves the destination a UDP flow is bound for. The
// relay target is the ORIGINAL hostname, not local.IP — for a fake-IP
// routed name local.IP is a 198.18.x.x handle that only means something on
// this machine, so a datagram addressed to it dies at the proxy's egress.
func (pf *proxyForwarder) udpDestination(localIP net.IP) (proxy.Entry, string, bool) {
	entry, ok := pf.table.Lookup(localIP)
	if !ok {
		entry, ok = pf.lookupIPRule(localIP)
		if !ok {
			return proxy.Entry{}, "", false
		}
	}
	target := entry.Hostname
	if target == "" {
		target = localIP.String()
	}
	return entry, target, true
}

// admitUDP is the netstack admission filter. Returning false makes the
// stack answer ICMP port-unreachable, which is the entire value of doing
// this here rather than in handleUDP: a destination we already know
// black-holes UDP gets refused on its first packet, so the client falls
// back to TCP immediately instead of stalling on a QUIC handshake.
func (pf *proxyForwarder) admitUDP(local, remote *net.UDPAddr) bool {
	if pf.health == nil {
		return true
	}
	_, target, ok := pf.udpDestination(local.IP)
	if !ok {
		return true // unmapped flows are handleUDP's to log and drop
	}
	return !pf.health.blocked(udpHealthKey(target, local.Port))
}

func (pf *proxyForwarder) handleUDP(conn net.Conn, local, remote *net.UDPAddr) {
	defer conn.Close()

	entry, target, ok := pf.udpDestination(local.IP)
	if !ok {
		pf.logger.Printf("proxytun/udp: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
		return
	}
	healthKey := udpHealthKey(target, local.Port)

	dctx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
	// Associating walks the same binding with the same retry budget as a
	// TCP dial, so it draws on the same ceiling. Released before the relay
	// pumps start, for the reason the TCP path releases before splicing.
	release, got := pf.gate.acquire(dctx)
	if !got {
		cancel()
		if ok, n := pf.sampler.allow(fmt.Sprintf("gate/udp|%s|%d", target, local.Port)); ok {
			pf.logger.Printf("proxytun/udp: %s:%d — dial ceiling (%d) saturated; dropping (+%d more suppressed)", target, local.Port, proxyMaxConcurrentDials, n)
		}
		return
	}
	session, used, err := pf.associateUDP(dctx, entry, nil)
	release()
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
	pf.logVia("proxytun/udp", target, local.Port, used)

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
	if sent > 0 {
		pf.stats.udpFlow(recv == 0)
	}
	if sent > 0 && recv == 0 {
		why := "relay black hole?"
		if noReply.Load() {
			why = "every association stayed silent; client falls back to TCP"
		}
		// Strike the destination, not the proxy: the same proxy carries a
		// sibling flow fine seconds later, so what's dead is the path to
		// this host. Two in a row and admitUDP starts refusing outright.
		if penalty := pf.health.strike(healthKey); penalty > 0 {
			pf.logger.Printf("proxytun/udp: %s:%d — UDP refused for %s (ICMP unreachable; client uses TCP)",
				target, local.Port, penalty)
		}
		pf.logger.Printf("proxytun/udp: %s:%d closed via %q — sent %s, RECEIVED NOTHING in %s (%s)",
			target, local.Port, nameChain(), humanBytes(sent), dur, why)
		return
	}
	pf.health.success(healthKey)
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
				pf.noteUpstreamFailure(entry, name)
				continue
			}
			// Only the FIRST association of a flow records the destination's
			// upstream. A re-association is driven by silence, and silence on
			// UDP is a property of the path to this host rather than of the
			// proxy (the same proxy carries a sibling flow fine seconds
			// later) — so it must not drag this destination's TCP
			// connections onto a different exit as well.
			if len(tried) == 0 {
				pf.sticky.Set(stickyKey(entry), name)
				pf.sticky.Set(siteKey(entry.Hostname), name)
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
func startProxyTunnel(store *proxy.Store, table *proxy.Table, router *routing.Manager, decider ipRouteDecider, latency *netprobe.LatencyTracker, traffic *trafficAggregator, stats *connStats, routes *routeKeys, logger *log.Logger) (*proxytun.Tunnel, string, *proxyForwarder) {
	tun, err := proxytun.Open(proxyUTUNAddr, 1500)
	if err != nil {
		logger.Printf("em-walld: proxy utun open failed (proxy routing disabled): %v", err)
		return nil, "", nil
	}
	fwd := &proxyForwarder{
		store:   store,
		table:   table,
		router:  router,
		decider: decider,
		latency: latency,
		sticky:  newStickyBindings(),
		traffic: traffic,
		health:  newUDPHealth(),
		breaker: newTCPHealth(),
		gate:    newDialGate(proxyMaxConcurrentDials),
		sampler: newLogSampler(),
		witness: newUplinkWitness(),
		stats:   stats,
		routes:  routes,
		logger:  logger,
	}
	tunnel, err := proxytun.NewTunnel(tun, 1500, fwd.handle, fwd.handleUDP, logger)
	if err != nil {
		logger.Printf("em-walld: proxy tunnel init failed (proxy routing disabled): %v", err)
		_ = tun.Close()
		return nil, "", nil
	}
	// Must be set before Start: a destination whose UDP relay is known dead
	// is refused with ICMP rather than swallowed, so the client never waits
	// out a QUIC handshake it can't win.
	tunnel.SetUDPFilter(fwd.admitUDP)
	logger.Printf("em-walld: proxy tunnel up on %s (addr %s)", tunnel.IfaceName(), proxyUTUNAddr)
	return tunnel, tunnel.IfaceName(), fwd
}
