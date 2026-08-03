package main

import (
	"context"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
)

// These drive handleUDP directly over a net.Pipe standing in for the netstack
// UDP endpoint, against stub SOCKS5 proxies. The point is the re-association
// path: a dead association (one that accepts UDP ASSOCIATE and then silently
// swallows every datagram) is exactly what was costing 90 seconds per flow in
// production, and it can't be reproduced by the core/proxy tests because it
// only exists at this layer.

// startStubSOCKS5UDP runs a SOCKS5 proxy that answers UDP ASSOCIATE. When
// echo is true its relay bounces each payload back upper-cased; when false it
// accepts datagrams and drops them — the "dead association" this file is
// about.
func startStubSOCKS5UDP(t *testing.T, echo bool) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub socks listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("stub relay listen: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	relayAddr := relay.LocalAddr().(*net.UDPAddr)

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, client, err := relay.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !echo {
				continue // dead association: swallow it
			}
			payload, perr := stubStripUDPHeader(buf[:n])
			if perr != nil {
				continue
			}
			out := []byte{0x00, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0} // v4 0.0.0.0:0
			out = append(out, []byte(strings.ToUpper(string(payload)))...)
			_, _ = relay.WriteToUDP(out, client)
		}
	}()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if err := stubServeAssociate(conn, relayAddr); err != nil {
					return
				}
				// Association lives as long as the control conn.
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return h, p
}

// stubServeAssociate performs no-auth negotiation and replies to ASSOCIATE
// with the relay address.
func stubServeAssociate(c net.Conn, relayAddr *net.UDPAddr) error {
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
	reply := []byte{0x05, 0x00, 0x00, 0x01}
	reply = append(reply, relayAddr.IP.To4()...)
	reply = append(reply, byte(relayAddr.Port>>8), byte(relayAddr.Port))
	_ = c.SetReadDeadline(time.Time{})
	_, err := c.Write(reply)
	return err
}

// stubStripUDPHeader returns the payload of a SOCKS5 UDP request datagram.
func stubStripUDPHeader(d []byte) ([]byte, error) {
	if len(d) < 10 {
		return nil, io.ErrUnexpectedEOF
	}
	off := 4
	switch d[3] {
	case 0x01:
		off += 4
	case 0x04:
		off += 16
	case 0x03:
		off += 1 + int(d[off])
	default:
		return nil, io.ErrUnexpectedEOF
	}
	off += 2
	if off > len(d) {
		return nil, io.ErrUnexpectedEOF
	}
	return d[off:], nil
}

// udpTestForwarder wires a proxyForwarder over a real proxy.Store holding the
// named stub proxies, with a table entry binding fakeIP to them in order.
func udpTestForwarder(t *testing.T, fakeIP net.IP, host string, names []string, ports map[string]int) *proxyForwarder {
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
		store:  store,
		table:  table,
		logger: log.New(io.Discard, "", 0),
	}
}

// compressUDPTimers shrinks the watchdog timeline so a test doesn't sit
// through the production 6-second grace period. Call it FIRST in a test: its
// cleanup restores the globals, and cleanups run LIFO, so every flow started
// afterwards is guaranteed to have finished before the restore happens.
func compressUDPTimers(t *testing.T) {
	t.Helper()
	noReply, tick := proxyUDPNoReplyTimeout, proxyUDPMonitorTick
	proxyUDPNoReplyTimeout = 300 * time.Millisecond
	proxyUDPMonitorTick = 20 * time.Millisecond
	t.Cleanup(func() {
		proxyUDPNoReplyTimeout, proxyUDPMonitorTick = noReply, tick
	})
}

// startFlow hands one UDP flow to handleUDP over a net.Pipe standing in for
// the netstack endpoint. The returned conn is the client side; done closes
// when handleUDP returns. Cleanup closes the client and waits for the flow to
// unwind, so no goroutine outlives the test.
func startFlow(t *testing.T, pf *proxyForwarder, fakeIP net.IP, clientPort int) (net.Conn, <-chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pf.handleUDP(server,
			&net.UDPAddr{IP: fakeIP, Port: 443},
			&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: clientPort})
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("handleUDP never returned after the client went away")
		}
	})
	return client, done
}

// pump stands in for QUIC retransmits: keep sending until the test ends.
func pump(t *testing.T, client net.Conn) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = client.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			if _, err := client.Write([]byte("ping")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// A flow whose first association silently swallows everything must move to
// the next proxy in the binding and start carrying traffic — without the
// client doing anything but retransmitting, which is all QUIC does.
func TestHandleUDP_ReassociatesPastDeadProxy(t *testing.T) {
	compressUDPTimers(t)

	deadHost, deadPort := startStubSOCKS5UDP(t, false)
	liveHost, livePort := startStubSOCKS5UDP(t, true)
	_, _ = deadHost, liveHost

	fakeIP := net.IPv4(198, 18, 0, 7)
	pf := udpTestForwarder(t, fakeIP, "example.com",
		[]string{"dead", "live"}, map[string]int{"dead": deadPort, "live": livePort})

	client, _ := startFlow(t, pf, fakeIP, 51000)
	pump(t, client)

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("no datagram ever came back — re-association failed: %v", err)
	}
	if got := string(buf[:n]); got != "PING" {
		t.Fatalf("got %q, want PING", got)
	}
}

// When every candidate is dead the flow must give up rather than hang: the
// client needs the endpoint freed so it can fall back to TCP.
func TestHandleUDP_GivesUpWhenAllAssociationsSilent(t *testing.T) {
	compressUDPTimers(t)

	_, deadPort := startStubSOCKS5UDP(t, false)

	fakeIP := net.IPv4(198, 18, 0, 8)
	pf := udpTestForwarder(t, fakeIP, "example.com",
		[]string{"dead"}, map[string]int{"dead": deadPort})

	client, done := startFlow(t, pf, fakeIP, 51001)
	pump(t, client)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleUDP never tore down a flow that heard nothing back")
	}
}

// A flow that works from the start must not be disturbed by the watchdog.
func TestHandleUDP_HealthyFlowIsNotReassociated(t *testing.T) {
	compressUDPTimers(t)

	_, livePort := startStubSOCKS5UDP(t, true)

	fakeIP := net.IPv4(198, 18, 0, 9)
	pf := udpTestForwarder(t, fakeIP, "example.com",
		[]string{"live"}, map[string]int{"live": livePort})

	client, _ := startFlow(t, pf, fakeIP, 51002)

	for i := 0; i < 3; i++ {
		_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write([]byte("ping")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := string(buf[:n]); got != "PING" {
			t.Fatalf("read %d: got %q, want PING", i, got)
		}
	}
}
