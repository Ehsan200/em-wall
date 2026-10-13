package dnsproxy

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// startMockResolver runs a UDP DNS server on an ephemeral loopback port and
// returns its "host:port" plus a shutdown func.
func startMockResolver(t *testing.T, h dns.HandlerFunc) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: h}
	go func() { _ = srv.ActivateAndServe() }()
	return pc.LocalAddr().String(), func() { _ = srv.Shutdown() }
}

func servfailHandler(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

func nxdomainHandler(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeNameError)
	_ = w.WriteMsg(m)
}

func aHandler(ip string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		rr, _ := dns.NewRR(r.Question[0].Name + " 60 IN A " + ip)
		m.Answer = []dns.RR{rr}
		_ = w.WriteMsg(m)
	}
}

func aQuery(name string) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return q
}

func answerIP(resp *dns.Msg) string {
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String()
		}
	}
	return ""
}

// A SERVFAIL from the first upstream must not be returned verbatim — we
// fail over to the next upstream, which answers.
func TestMultiUpstream_FailsOverPastServfail(t *testing.T) {
	bad, stopBad := startMockResolver(t, servfailHandler)
	defer stopBad()
	good, stopGood := startMockResolver(t, aHandler("192.0.2.1"))
	defer stopGood()

	mu := NewMultiUpstream([]string{bad, good}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("service.test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if got := answerIP(resp); got != "192.0.2.1" {
		t.Errorf("answer = %q, want 192.0.2.1", got)
	}
}

// A single upstream that SERVFAILs once then recovers must be retried, since
// the failure is transient — this is the symptom we saw in the wild.
func TestMultiUpstream_RetriesTransientServfail(t *testing.T) {
	var n int32
	flaky, stop := startMockResolver(t, func(w dns.ResponseWriter, r *dns.Msg) {
		if atomic.AddInt32(&n, 1) == 1 {
			servfailHandler(w, r)
			return
		}
		aHandler("192.0.2.2")(w, r)
	})
	defer stop()

	mu := NewMultiUpstream([]string{flaky}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("service.test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := answerIP(resp); got != "192.0.2.2" {
		t.Errorf("answer = %q, want 192.0.2.2 (retry should have recovered)", got)
	}
}

// When every upstream only ever SERVFAILs, surface that SERVFAIL (not a
// transport error) so the client gets a real DNS rcode.
func TestMultiUpstream_AllServfailReturnsServfail(t *testing.T) {
	s1, stop1 := startMockResolver(t, servfailHandler)
	defer stop1()
	s2, stop2 := startMockResolver(t, servfailHandler)
	defer stop2()

	mu := NewMultiUpstream([]string{s1, s2}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("service.test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
}

// A public resolver may NXDOMAIN a geo-split / on-path-tampered name (e.g. a
// country-code host that only resolves in-region) while another upstream
// resolves it. The real answer must win even when the NXDOMAIN upstream is
// listed FIRST — NXDOMAIN with no answer section though the name exists.
// Before the rank-based fix, the first NXDOMAIN was treated as definitive.
func TestMultiUpstream_PositiveBeatsNXDomain(t *testing.T) {
	nx, stopNX := startMockResolver(t, nxdomainHandler)
	defer stopNX()
	good, stopGood := startMockResolver(t, aHandler("192.0.2.9"))
	defer stopGood()

	mu := NewMultiUpstream([]string{nx, good}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("geo-split.test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if got := answerIP(resp); got != "192.0.2.9" {
		t.Errorf("answer = %q, want 192.0.2.9 (a real answer must beat NXDOMAIN)", got)
	}
}

// When every upstream agrees the name does not exist, return NXDOMAIN.
func TestMultiUpstream_AllNXDomainReturnsNXDomain(t *testing.T) {
	s1, stop1 := startMockResolver(t, nxdomainHandler)
	defer stop1()
	s2, stop2 := startMockResolver(t, nxdomainHandler)
	defer stop2()

	mu := NewMultiUpstream([]string{s1, s2}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("nonexistent.invalid"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %s, want NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
}

// blackholeResolver reads queries and never answers — a dead or filtered
// upstream (observed: a phone-hotspot resolver, DNS-intercepted 1.1.1.1).
// hits counts queries it received.
func blackholeResolver(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	var hits atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			hits.Add(1)
		}
	}()
	return pc.LocalAddr().String(), &hits
}

// Three dead upstreams ahead of the one that answers: the answer must come
// back well inside the query's 5s budget. A serial walk spent 3 x 3s on
// the dead ones and never reached it.
func TestMultiUpstream_DeadUpstreamsDontStarveLiveOne(t *testing.T) {
	d1, _ := blackholeResolver(t)
	d2, _ := blackholeResolver(t)
	d3, _ := blackholeResolver(t)
	good, stop := startMockResolver(t, aHandler("192.0.2.7"))
	defer stop()

	mu := NewMultiUpstream([]string{d1, d2, d3, good}, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := mu.Forward(ctx, aQuery("ip-api.test"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got := answerIP(resp); got != "192.0.2.7" {
		t.Fatalf("answer = %q, want 192.0.2.7", got)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("took %v; dead upstreams were waited out", el)
	}
}

// The upstream that answered is asked first next time, so a dead
// first-listed resolver stops costing every query a hedge delay.
func TestMultiUpstream_PrefersLastAnswerer(t *testing.T) {
	dead, hits := blackholeResolver(t)
	good, stop := startMockResolver(t, aHandler("192.0.2.8"))
	defer stop()

	mu := NewMultiUpstream([]string{dead, good}, 3*time.Second)
	if _, err := mu.Forward(context.Background(), aQuery("a.test")); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	start := time.Now()
	if _, err := mu.Forward(context.Background(), aQuery("b.test")); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el >= upstreamHedgeDelay {
		t.Fatalf("second query took %v — preferred upstream not asked first", el)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != before {
		t.Fatal("dead upstream asked although the preferred one answered at once")
	}
}

// A definitive negative must not wait out a dead upstream's full timeout.
func TestMultiUpstream_NegativeDoesNotWaitForDeadUpstream(t *testing.T) {
	dead, _ := blackholeResolver(t)
	nx, stop := startMockResolver(t, nxdomainHandler)
	defer stop()

	mu := NewMultiUpstream([]string{dead, nx}, 3*time.Second)
	start := time.Now()
	resp, err := mu.Forward(context.Background(), aQuery("nope.test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %s, want NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}
	if el := time.Since(start); el > upstreamHedgeDelay+upstreamNegativeGrace+500*time.Millisecond {
		t.Fatalf("took %v; waited out the dead upstream", el)
	}
}
