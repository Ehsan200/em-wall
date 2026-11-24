package main

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

func TestChangedPathEntries(t *testing.T) {
	plan := livePlan{
		rmOut:  []string{"slot0-out-node1", "out-e", "dialer-m2", "blackhole"},
		addOut: []string{"slot1-out-new", "out-added"}, // additions leave open streams alone
		rmIn:   []string{"in-f"},
	}
	old := []xray.DialerSlot{
		{Master: "m", Aliases: []string{"m3"}, Index: 0},
		{Master: "n", Index: 1},
	}
	got := changedPathEntries(plan, old)
	want := []string{"m", "m3", "e", "m2", "f"}
	for _, w := range want {
		if !got[xray.InternalProxyName(w)] {
			t.Errorf("%s not marked; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want exactly %v", got, want)
	}
}

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

// Only a connection that is BOTH on a changed path AND waiting on a reply
// is closed. A stale one that answers, a stalled one on an untouched path
// and a stale idle one all stay open.
func TestStaleSweepClosesOnlyStalledStaleConns(t *testing.T) {
	l := newLiveConns(nil)
	type conn struct {
		tracked net.Conn
		far     net.Conn // upstream's far end
		client  net.Conn
	}
	mk := func(name string) conn {
		up, far := tcpPair(t)
		client, _ := tcpPair(t)
		tr, untrack := l.track(name, client, up)
		t.Cleanup(untrack)
		return conn{tr, far, client}
	}
	stalled := mk("_xray_m")
	answered := mk("_xray_m")
	untouched := mk("_xray_other")
	idle := mk("_xray_m")

	for _, c := range []conn{stalled, answered, untouched} {
		if _, err := c.tracked.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
	}
	// answered gets its reply.
	buf := make([]byte, 4)
	_, _ = io.ReadFull(answered.far, buf)
	_, _ = answered.far.Write([]byte("pong"))
	_, _ = io.ReadFull(answered.tracked, buf)

	if n := l.markStale(map[string]bool{"_xray_m": true}); n != 3 {
		t.Fatalf("marked %d, want 3", n)
	}
	if n := l.sweep(time.Now()); n != 0 {
		t.Fatalf("swept %d before the stall timeout", n)
	}
	if n := l.sweep(time.Now().Add(stalePathStallTimeout + time.Second)); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		for { // drain the unanswered ping, then see EOF or a timeout
			if _, err := c.Read(buf); err != nil {
				ne, ok := err.(net.Error)
				return !(ok && ne.Timeout())
			}
		}
	}
	if !closed(stalled.far) {
		t.Error("stalled stale connection left open")
	}
	for name, c := range map[string]conn{"answered": answered, "untouched": untouched, "idle": idle} {
		if closed(c.far) {
			t.Errorf("%s connection was closed", name)
		}
	}
}

// startDyingSOCKS5 is a SOCKS5 upstream that answers the opening exchange
// and then goes silent — the old path after its member died.
func startDyingSOCKS5(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if stubServeConnect(c, stubTCPHealthy) != nil {
					return
				}
				buf := make([]byte, 4096)
				if _, err := c.Read(buf); err != nil {
					return
				}
				if _, err := c.Write(stubServerHello); err != nil {
					return
				}
				for { // swallow everything, answer nothing
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	return port
}

// End to end through the splice: after a path change, a connection whose
// upstream stops answering is closed so the client reconnects, while one
// whose upstream keeps answering is left alone.
func TestStalePathClosesStalledSplice(t *testing.T) {
	compressTCPTimers(t)
	prev := stalePathStallTimeout
	stalePathStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stalePathStallTimeout = prev })

	ports := map[string]int{
		"_xray_dying":   startDyingSOCKS5(t),
		"_xray_healthy": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	start := func(name string, ip net.IP) (net.Conn, *liveConns, chan struct{}) {
		pf := tcpTestForwarder(t, ip, "example.com", []string{name}, ports)
		pf.live = newLiveConns(nil)
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		done := make(chan struct{})
		go func() {
			defer close(done)
			pf.handle(server, &net.TCPAddr{IP: ip, Port: 443}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000})
		}()
		if _, err := client.Write(clientHello); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(stubServerHello))
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(client, buf); err != nil {
			t.Fatalf("%s: no opening reply: %v", name, err)
		}
		_ = client.SetReadDeadline(time.Time{})
		return client, pf.live, done
	}

	for _, tc := range []struct {
		name      string
		ip        net.IP
		wantClose bool
	}{
		{"_xray_dying", net.IPv4(198, 18, 0, 40), true},
		{"_xray_healthy", net.IPv4(198, 18, 0, 41), false},
	} {
		client, live, done := start(tc.name, tc.ip)
		if n := live.markStale(map[string]bool{tc.name: true}); n != 1 {
			t.Fatalf("%s: marked %d, want 1", tc.name, n)
		}
		go func() { _, _ = client.Write(clientHello) }()
		if !tc.wantClose {
			buf := make([]byte, len(stubServerHello))
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(client, buf); err != nil {
				t.Fatalf("%s: healthy stream broke: %v", tc.name, err)
			}
		}
		time.Sleep(stalePathStallTimeout + 100*time.Millisecond)
		n := live.sweep(time.Now())
		if tc.wantClose {
			if n != 1 {
				t.Fatalf("%s: swept %d, want 1", tc.name, n)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s: splice still running after the stalled connection was closed", tc.name)
			}
		} else if n != 0 {
			t.Fatalf("%s: swept %d healthy connection(s)", tc.name, n)
		}
	}
}
