package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/proxytun"
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
	logger *log.Logger
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

	ctx, cancel := context.WithTimeout(context.Background(), proxy.DefaultDialTimeout)
	defer cancel()

	// Walk the rule's proxy binding in order, using the first that both
	// exists and dials successfully — same first-available fallback as
	// app:KEY1,KEY2.
	var (
		upstream net.Conn
		used     string
		lastErr  error
	)
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
		c, err := dialer.Dial(ctx, entry.Hostname, local.IP, local.Port)
		if err != nil {
			lastErr = err
			continue
		}
		upstream, used = c, name
		break
	}
	if upstream == nil {
		pf.logger.Printf("proxytun: %s:%d — all upstream proxies failed: %v", target, local.Port, lastErr)
		return
	}
	defer upstream.Close()

	pf.logger.Printf("proxytun: %s:%d via proxy %q", target, local.Port, used)
	_, _, _ = proxy.Splice(conn, upstream)
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

	dctx, cancel := context.WithTimeout(context.Background(), proxy.DefaultDialTimeout)
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

	pf.logger.Printf("proxytun/udp: %s:%d via proxy %q", local.IP, local.Port, used)

	// Three goroutines: client→relay, relay→client, and a watcher that
	// trips when the SOCKS5 control conn closes (which invalidates the
	// association). Buffer 3 so all can send their done signal without
	// blocking after the first teardown.
	done := make(chan struct{}, 3)

	go func() {
		buf := make([]byte, 64*1024)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(proxyUDPIdleTimeout))
			n, rerr := conn.Read(buf)
			if n > 0 {
				if werr := session.WriteTo(buf[:n], &net.UDPAddr{IP: local.IP, Port: local.Port}); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		buf := make([]byte, 64*1024)
		for {
			_ = session.SetReadDeadline(time.Now().Add(proxyUDPIdleTimeout))
			n, rerr := session.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		// Parks until the control conn hits EOF/close — either the
		// proxy dropped the association or our own Close() below did.
		b := make([]byte, 1)
		_, _ = session.CtrlConn().Read(b)
		done <- struct{}{}
	}()

	<-done
	// First exit wins; closing both ends unblocks the other pumps so
	// they error out and exit too.
	_ = conn.Close()
	_ = session.Close()
}

// associateUDP walks the rule's proxy binding and returns the first
// SOCKS5 proxy for which a UDP ASSOCIATE succeeds. HTTP proxies are
// skipped (no UDP support). Returns (nil, "", lastErr) if none work.
func (pf *proxyForwarder) associateUDP(ctx context.Context, entry proxy.Entry) (*proxy.UDPSession, string, error) {
	var lastErr error
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
		sess, err := proxy.DialUDPAssociate(ctx, p)
		if err != nil {
			lastErr = err
			continue
		}
		return sess, name, nil
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
func startProxyTunnel(store *proxy.Store, table *proxy.Table, logger *log.Logger) (*proxytun.Tunnel, string) {
	tun, err := proxytun.Open(proxyUTUNAddr, 1500)
	if err != nil {
		logger.Printf("em-walld: proxy utun open failed (proxy routing disabled): %v", err)
		return nil, ""
	}
	fwd := &proxyForwarder{store: store, table: table, logger: logger}
	tunnel, err := proxytun.NewTunnel(tun, 1500, fwd.handle, fwd.handleUDP, logger)
	if err != nil {
		logger.Printf("em-walld: proxy tunnel init failed (proxy routing disabled): %v", err)
		_ = tun.Close()
		return nil, ""
	}
	logger.Printf("em-walld: proxy tunnel up on %s (addr %s)", tunnel.IfaceName(), proxyUTUNAddr)
	return tunnel, tunnel.IfaceName()
}
