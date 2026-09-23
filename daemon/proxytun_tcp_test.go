package main

import (
	"context"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
)

// These cover the TCP dial path's failover. The case that matters is the one
// no dial error reports: an xray entry is a loopback SOCKS5 inbound that
// answers CONNECT with success before it has dialed anything, so a dead node
// looks identical to a healthy one until the bytes don't come back.
// stubTCPMode reproduces that shape directly.

type stubTCPMode int

const (
	// stubTCPHealthy completes the handshake and answers the client's first
	// bytes — what a working upstream does.
	stubTCPHealthy stubTCPMode = iota
	// stubTCPSilent grants CONNECT and then never sends anything: the
	// early-reply black hole an xray entry with a dead outbound produces.
	stubTCPSilent
	// stubTCPRefuse fails the CONNECT itself (a proxy that at least admits
	// it can't reach the destination).
	stubTCPRefuse
	// stubTCPSlow answers like stubTCPHealthy, but only after stubSlowDelay:
	// a path that works and is merely slow.
	stubTCPSlow
)

var stubSlowDelay = 600 * time.Millisecond

// stubServerHello is what stubTCPHealthy answers with — shaped like a TLS
// ServerHello record so the test data matches what the verification is
// actually looking at.
var stubServerHello = []byte{0x16, 0x03, 0x03, 0x00, 0x04, 'o', 'k', '!', '?'}

// startStubSOCKS5TCP runs a SOCKS5 CONNECT proxy behaving per mode and
// returns its port.
func startStubSOCKS5TCP(t *testing.T, mode stubTCPMode) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub socks listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if err := stubServeConnect(conn, mode); err != nil {
					return
				}
				if mode == stubTCPRefuse {
					return
				}
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 && mode == stubTCPSlow {
						time.Sleep(stubSlowDelay)
					}
					if n > 0 && (mode == stubTCPHealthy || mode == stubTCPSlow) {
						if _, werr := conn.Write(stubServerHello); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	_, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return p
}

// stubServeConnect performs no-auth negotiation and answers the CONNECT
// request — with success for every mode but stubTCPRefuse, exactly like an
// xray socks inbound, which replies before it knows whether the far side is
// reachable.
func stubServeConnect(c net.Conn, mode stubTCPMode) error {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
		return err
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return err
	}
	switch req[3] {
	case 0x01:
		_, _ = io.ReadFull(c, make([]byte, 4+2))
	case 0x04:
		_, _ = io.ReadFull(c, make([]byte, 16+2))
	case 0x03:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		_, _ = io.ReadFull(c, make([]byte, int(l[0])+2))
	}
	rep := byte(0x00)
	if mode == stubTCPRefuse {
		rep = 0x05 // connection refused
	}
	_ = c.SetReadDeadline(time.Time{})
	_, err := c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

// tcpTestForwarder wires a forwarder over a store holding the named stub
// proxies, with a table entry binding fakeIP to them in the given order.
func tcpTestForwarder(t *testing.T, fakeIP net.IP, host string, names []string, ports map[string]int) *proxyForwarder {
	t.Helper()
	store, err := proxy.Open(filepath.Join(t.TempDir(), "proxies.db"))
	if err != nil {
		t.Fatalf("open proxy store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, n := range names {
		if _, err := store.Add(context.Background(), proxy.Proxy{
			Name: n, Protocol: proxy.ProtocolSOCKS5, Host: "127.0.0.1", Port: ports[n],
		}); err != nil {
			t.Fatalf("add proxy %s: %v", n, err)
		}
	}
	table := proxy.NewTable(time.Minute)
	table.Record(fakeIP, host, names, time.Minute, 1)
	return &proxyForwarder{
		store:   store,
		table:   table,
		latency: netprobe.NewLatencyTracker(time.Minute),
		sticky:  newStickyBindings(),
		logger:  log.New(io.Discard, "", 0),
	}
}

// compressTCPTimers shrinks the opening-exchange waits so a test doesn't sit
// through the production first-byte grace period.
func compressTCPTimers(t *testing.T) {
	t.Helper()
	hello, first := proxyClientHelloWait, proxyFirstByteTimeout
	proxyClientHelloWait = 200 * time.Millisecond
	proxyFirstByteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { proxyClientHelloWait, proxyFirstByteTimeout = hello, first })
}

// clientHello is a minimal byte string with the TLS handshake record shape
// dialBinding keys verification off.
var clientHello = []byte{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'}

// dialThrough drives dialBinding with the given client opening bytes and
// returns what it selected.
func dialThrough(t *testing.T, pf *proxyForwarder, fakeIP net.IP, opening []byte) (net.Conn, string, int64, error) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go func() {
		if len(opening) > 0 {
			_, _ = client.Write(opening)
		}
	}()
	entry, ok := pf.table.Lookup(fakeIP)
	if !ok {
		t.Fatalf("table lookup %s failed", fakeIP)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up, used, sent, err := pf.dialBinding(ctx, entry, &net.TCPAddr{IP: fakeIP, Port: 443}, server)
	if up != nil {
		t.Cleanup(func() { _ = up.Close() })
	}
	return up, used, sent, err
}

// A member that grants CONNECT and then stays silent is exactly what a dead
// xray outbound looks like from the dial path. Without first-byte
// verification the binding stops at it and the connection black-holes.
func TestDialBindingFailsOverSilentUpstream(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{
		"dead": startStubSOCKS5TCP(t, stubTCPSilent),
		"good": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 7)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"dead", "good"}, ports)

	up, used, sent, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil || up == nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if used != "good" {
		t.Fatalf("used = %q, want good", used)
	}
	if sent != int64(len(clientHello)) {
		t.Fatalf("sent = %d, want %d", sent, len(clientHello))
	}
}

// The bytes read ahead to prove the path works still have to reach the
// client — they're the start of the TLS handshake, not a probe.
func TestDialBindingReplaysServerPrefix(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{"good": startStubSOCKS5TCP(t, stubTCPHealthy)}
	fakeIP := net.IPv4(198, 18, 0, 8)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"good"}, ports)

	up, _, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil || up == nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	got := make([]byte, len(stubServerHello))
	_ = up.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(up, got); err != nil {
		t.Fatalf("read server prefix: %v", err)
	}
	if string(got) != string(stubServerHello) {
		t.Fatalf("server prefix = %q, want %q", got, stubServerHello)
	}
}

// Silence is only proof of a dead path for TLS, where the peer owes a reply
// within an RTT. Everything else is forwarded unverified rather than risk
// tearing down a legitimately quiet protocol (long-poll, server-first).
func TestDialBindingSkipsVerificationForNonTLS(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{
		"first":  startStubSOCKS5TCP(t, stubTCPSilent),
		"second": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 9)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"first", "second"}, ports)

	_, used, _, err := dialThrough(t, pf, fakeIP, []byte("GET / HTTP/1.1\r\n\r\n"))
	if err != nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if used != "first" {
		t.Fatalf("used = %q, want first (non-TLS is not verified)", used)
	}
}

// A CONNECT the proxy actually refuses has always failed over; make sure the
// new opening-exchange code didn't break the plain case.
func TestDialBindingFailsOverRefusedConnect(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{
		"refuser": startStubSOCKS5TCP(t, stubTCPRefuse),
		"good":    startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 10)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"refuser", "good"}, ports)

	_, used, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if used != "good" {
		t.Fatalf("used = %q, want good", used)
	}
}

// Every member silent means there is nothing to hand the client — better an
// immediate close than a connection that will never carry anything.
func TestDialBindingAllSilentFails(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{
		"a": startStubSOCKS5TCP(t, stubTCPSilent),
		"b": startStubSOCKS5TCP(t, stubTCPSilent),
	}
	fakeIP := net.IPv4(198, 18, 0, 11)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"a", "b"}, ports)

	up, _, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if up != nil {
		t.Fatalf("expected no upstream, got one")
	}
	if err == nil {
		t.Fatalf("expected an error")
	}
}

// The destination remembers which upstream carried it, and a member that
// black-holed a connection takes a strike so ranking can demote it.
func TestDialBindingRecordsStickyAndStrikes(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{
		"dead": startStubSOCKS5TCP(t, stubTCPSilent),
		"good": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 12)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"dead", "good"}, ports)

	if _, _, _, err := dialThrough(t, pf, fakeIP, clientHello); err != nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if got := pf.sticky.Get("example.com"); got != "good" {
		t.Fatalf("sticky = %q, want good", got)
	}
	// One strike isn't a verdict, so a second failure is what has to move
	// "dead" behind an unprobed name.
	pf.latency.Fail("dead")
	if got := pf.latency.Rank([]string{"dead", "unprobed"}); got[0] != "unprobed" {
		t.Fatalf("rank = %v, want the struck member last", got)
	}
}

// Once a destination is on an upstream it stays there while that upstream is
// healthy — the whole point of sticky selection. A raw latency sort would put
// the marginally-faster member first and scatter the site across exits.
func TestOrderedNamesPrefersStickyIncumbent(t *testing.T) {
	pf := &proxyForwarder{
		latency: netprobe.NewLatencyTracker(time.Minute),
		sticky:  newStickyBindings(),
		logger:  log.New(io.Discard, "", 0),
	}
	entry := proxy.Entry{Hostname: "example.com", ProxyNames: []string{"a", "b"}}

	pf.latency.Record("a", 200*time.Millisecond, true)
	pf.latency.Record("b", 185*time.Millisecond, true)
	if got := pf.orderedNames(entry); got[0] != "b" {
		t.Fatalf("with no incumbent, order = %v, want b first", got)
	}
	pf.sticky.Set("example.com", "a")
	if got := pf.orderedNames(entry); got[0] != "a" {
		t.Fatalf("with incumbent a, order = %v, want a first", got)
	}
}
