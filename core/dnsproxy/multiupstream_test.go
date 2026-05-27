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
	good, stopGood := startMockResolver(t, aHandler("1.2.3.4"))
	defer stopGood()

	mu := NewMultiUpstream([]string{bad, good}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if got := answerIP(resp); got != "1.2.3.4" {
		t.Errorf("answer = %q, want 1.2.3.4", got)
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
		aHandler("5.6.7.8")(w, r)
	})
	defer stop()

	mu := NewMultiUpstream([]string{flaky}, 2*time.Second)
	resp, err := mu.Forward(context.Background(), aQuery("example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := answerIP(resp); got != "5.6.7.8" {
		t.Errorf("answer = %q, want 5.6.7.8 (retry should have recovered)", got)
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
	resp, err := mu.Forward(context.Background(), aQuery("example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
}
