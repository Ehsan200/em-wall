package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/proxytun"
	"github.com/ehsan/em-wall/core/routing"
)

// proxyUDPIdleTimeout tears down a UDP flow's association after this
// much silence in both directions. UDP has no FIN, so this is the only
// way to reclaim a flow's relay socket + control conn. Long enough for
// chatty QUIC, short enough that one-shot DNS lookups don't linger.
const proxyUDPIdleTimeout = 60 * time.Second

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
	store  *proxy.Store
	table  *proxy.Table
	router *routing.Manager
	logger *log.Logger
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
		// The IP was routed to our utun but we have no DNS-time record
		// of which proxy to use — most likely a stale route lingering
		// after a table sweep. Drop the connection.
		pf.logger.Printf("proxytun: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
		return
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
	_, _, _ = proxy.Splice(conn, upstream)
}

// dialBinding walks the rule's proxy binding in order (first-available
// fallback) and retries the whole list with backoff on transient failure.
// Each dial is capped by proxyDialAttemptTimeout (clamped to remaining ctx).
func (pf *proxyForwarder) dialBinding(ctx context.Context, entry proxy.Entry, local *net.TCPAddr) (net.Conn, string, error) {
	var lastErr error
	for round := 1; round <= proxyDialMaxRounds; round++ {
		for _, name := range entry.ProxyNames {
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
		pf.logger.Printf("proxytun/udp: no proxy mapping for %s (from %s); dropping", local.IP, remote.IP)
		return
	}

	dctx, cancel := context.WithTimeout(context.Background(), proxyConnectDeadline)
	session, used, err := pf.associateUDP(dctx, entry)
	cancel()
	if session == nil {
		target := entry.Hostname
		if target == "" {
			target = local.IP.String()
		}
		pf.logger.Printf("proxytun/udp: %s:%d — no usable proxy: %v", target, local.Port, err)
		return
	}
	defer session.Close()

	stopKeepalive := pf.keepRouteAlive(local.IP)
	defer stopKeepalive()

	pf.logger.Printf("proxytun/udp: %s:%d via proxy %q", local.IP, local.Port, used)

	// Single idempotent teardown. Closing both ends unblocks every pump's
	// blocking Read so they error out and exit — no per-read deadlines.
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = session.Close()
		})
	}
	defer shutdown()

	// Idle is measured across BOTH directions: either pump bumps lastActive
	// on a datagram, so a flow that's busy one-way stays up. A monitor tears
	// it down only after proxyUDPIdleTimeout of total silence.
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())

	var wg sync.WaitGroup
	wg.Add(2)

	// client → relay
	go func() {
		defer wg.Done()
		defer shutdown()
		dst := &net.UDPAddr{IP: local.IP, Port: local.Port}
		buf := make([]byte, 64*1024)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				lastActive.Store(time.Now().UnixNano())
				if werr := session.WriteTo(buf[:n], dst); werr != nil {
					return
				}
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
			n, rerr := session.Read(buf)
			if n > 0 {
				lastActive.Store(time.Now().UnixNano())
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Watch the SOCKS5 control conn: its close means the proxy dropped the
	// association. Exits when shutdown closes ctrl.
	go func() {
		b := make([]byte, 1)
		_, _ = session.CtrlConn().Read(b)
		shutdown()
	}()

	// Idle monitor: tear down after total-silence exceeds the timeout.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(proxyUDPIdleTimeout / 2)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastActive.Load())) >= proxyUDPIdleTimeout {
					shutdown()
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
}

// associateUDP walks the rule's proxy binding and returns the first
// SOCKS5 proxy for which a UDP ASSOCIATE succeeds. HTTP proxies are
// skipped (no UDP support). Returns (nil, "", lastErr) if none work.
func (pf *proxyForwarder) associateUDP(ctx context.Context, entry proxy.Entry) (*proxy.UDPSession, string, error) {
	var lastErr error
	for round := 1; round <= proxyDialMaxRounds; round++ {
		for _, name := range entry.ProxyNames {
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
func startProxyTunnel(store *proxy.Store, table *proxy.Table, router *routing.Manager, logger *log.Logger) (*proxytun.Tunnel, string) {
	tun, err := proxytun.Open(proxyUTUNAddr, 1500)
	if err != nil {
		logger.Printf("em-walld: proxy utun open failed (proxy routing disabled): %v", err)
		return nil, ""
	}
	fwd := &proxyForwarder{store: store, table: table, router: router, logger: logger}
	tunnel, err := proxytun.NewTunnel(tun, 1500, fwd.handle, fwd.handleUDP, logger)
	if err != nil {
		logger.Printf("em-walld: proxy tunnel init failed (proxy routing disabled): %v", err)
		_ = tun.Close()
		return nil, ""
	}
	logger.Printf("em-walld: proxy tunnel up on %s (addr %s)", tunnel.IfaceName(), proxyUTUNAddr)
	return tunnel, tunnel.IfaceName()
}
