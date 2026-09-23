package main

import (
	"net"
	"testing"
	"time"
)

func compressHedge(t *testing.T, d time.Duration) {
	t.Helper()
	old := proxyHedgeDelay
	proxyHedgeDelay = d
	t.Cleanup(func() { proxyHedgeDelay = old })
}

// A stalled first member must not cost the full first-byte timeout: the
// next member starts beside it after the hedge delay and wins.
func TestDialBindingHedgesPastStalledMember(t *testing.T) {
	compressTCPTimers(t)
	proxyFirstByteTimeout = 5 * time.Second // the stall alone would take this long
	compressHedge(t, 50*time.Millisecond)
	ports := map[string]int{
		"stalled": startStubSOCKS5TCP(t, stubTCPSilent),
		"good":    startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 20)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"stalled", "good"}, ports)

	start := time.Now()
	_, used, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if used != "good" {
		t.Fatalf("used = %q, want good", used)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %s; hedging should not wait out the stalled member", el)
	}
}

// A slow-but-working path that answers after the hedge delay still wins
// when it is the only one that answers — the race extends patience, it
// doesn't shorten it.
func TestDialBindingSlowPathStillWins(t *testing.T) {
	compressTCPTimers(t)
	proxyFirstByteTimeout = 3 * time.Second
	compressHedge(t, 50*time.Millisecond)
	ports := map[string]int{
		"slow": startStubSOCKS5TCP(t, stubTCPSlow),
		"dead": startStubSOCKS5TCP(t, stubTCPSilent),
	}
	fakeIP := net.IPv4(198, 18, 0, 21)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"slow", "dead"}, ports)

	_, used, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if used != "slow" {
		t.Fatalf("used = %q, want slow", used)
	}
}

// When every member fails together the members are not to blame (local
// uplink or destination): none of them may take a strike, and a sticky
// binding must survive.
func TestDialBindingTotalFailureBlamesNoMember(t *testing.T) {
	compressTCPTimers(t)
	compressHedge(t, 20*time.Millisecond)
	ports := map[string]int{
		"a": startStubSOCKS5TCP(t, stubTCPSilent),
		"b": startStubSOCKS5TCP(t, stubTCPSilent),
	}
	fakeIP := net.IPv4(198, 18, 0, 22)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"a", "b"}, ports)
	pf.sticky.Set("example.com", "a")

	if up, _, _, _ := dialThrough(t, pf, fakeIP, clientHello); up != nil {
		t.Fatalf("expected no upstream")
	}
	for _, h := range pf.latency.Snapshot() {
		if h.Fails > 0 || h.Samples > 0 {
			t.Fatalf("member %q was struck on a total failure: %+v", h.Name, h)
		}
	}
	if got := pf.sticky.Get("example.com"); got != "a" {
		t.Fatalf("sticky = %q, want a (kept through a total failure)", got)
	}
}

// The sticky incumbent gets the hedge delay as a head start, so a healthy
// incumbent keeps the destination even when a sibling is also healthy.
func TestDialBindingIncumbentKeepsHeadStart(t *testing.T) {
	compressTCPTimers(t)
	compressHedge(t, 300*time.Millisecond)
	ports := map[string]int{
		"a": startStubSOCKS5TCP(t, stubTCPHealthy),
		"b": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 23)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"a", "b"}, ports)
	pf.latency.Record("a", 10*time.Millisecond, true)
	pf.latency.Record("b", 11*time.Millisecond, true)
	pf.sticky.Set("example.com", "b")

	for i := 0; i < 5; i++ {
		_, used, _, err := dialThrough(t, pf, fakeIP, clientHello)
		if err != nil {
			t.Fatalf("dialBinding failed: %v", err)
		}
		if used != "b" {
			t.Fatalf("dial %d used %q, want sticky incumbent b", i, used)
		}
	}
}

// Unverified connections still get the client's opening bytes forwarded:
// the healthy stub only answers once it has received something.
func TestDialBindingForwardsUnverifiedOpening(t *testing.T) {
	compressTCPTimers(t)
	ports := map[string]int{"good": startStubSOCKS5TCP(t, stubTCPHealthy)}
	fakeIP := net.IPv4(198, 18, 0, 24)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"good"}, ports)

	up, _, sent, err := dialThrough(t, pf, fakeIP, []byte("GET / HTTP/1.1\r\n\r\n"))
	if err != nil || up == nil {
		t.Fatalf("dialBinding failed: %v", err)
	}
	if sent == 0 {
		t.Fatalf("sent = 0, want the opening bytes billed")
	}
	buf := make([]byte, len(stubServerHello))
	_ = up.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := up.Read(buf); err != nil {
		t.Fatalf("upstream never answered — opening bytes were not forwarded: %v", err)
	}
}
