package dnsproxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/rules"
)

type fakeForwarder struct {
	mu    sync.Mutex
	resp  *dns.Msg
	err   error
	calls int
}

func (f *fakeForwarder) Forward(_ context.Context, req *dns.Msg) (*dns.Msg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.resp == nil {
		// Default: empty NOERROR with same question.
		r := new(dns.Msg)
		r.SetReply(req)
		return r, nil
	}
	r := f.resp.Copy()
	r.Id = req.Id
	r.Question = req.Question
	return r, nil
}

type fakeRoutes struct {
	mu    sync.Mutex
	calls []routeCall
	err   error
}

type routeCall struct {
	host, iface string
	ttl         time.Duration
	ruleID      int64
}

func (f *fakeRoutes) Install(_ context.Context, host, iface string, ttl time.Duration, ruleID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, routeCall{host, iface, ttl, ruleID})
	return f.err
}

type fakeLogs struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	name, action, iface, clientIP string
	ruleID                        int64
}

func (f *fakeLogs) Log(name, action, iface string, ruleID int64, clientIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, logEntry{name, action, iface, clientIP, ruleID})
}

type ruleSet []rules.Rule

func (r ruleSet) List(_ context.Context) ([]rules.Rule, error) { return []rules.Rule(r), nil }

func startServer(t *testing.T, ruleList []rules.Rule, fwd Forwarder, routes RouteInstaller, logs LogSink) (*Server, string) {
	t.Helper()
	eng := decision.New(ruleSet(ruleList))
	if err := eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Listen on random local port to avoid needing root.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	s, err := New(Config{
		Listen:    addr,
		Decider:   eng,
		Forwarder: fwd,
		Routes:    routes,
		Logs:      logs,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Start(ctx) }()

	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("server never became ready")
	}
	return s, addr
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: time.Second}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %q: %v", name, err)
	}
	return resp
}

func TestServer_BlocksMatching(t *testing.T) {
	fwd := &fakeForwarder{}
	logs := &fakeLogs{}
	rs := []rules.Rule{
		{ID: 1, Pattern: "*.bad.com", Action: rules.ActionBlock, Enabled: true},
	}
	_, addr := startServer(t, rs, fwd, nil, logs)

	resp := query(t, addr, "x.bad.com", dns.TypeA)
	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN, got %s", dns.RcodeToString[resp.Rcode])
	}
	if fwd.calls != 0 {
		t.Errorf("blocked query should not forward, got %d calls", fwd.calls)
	}
	if len(logs.entries) != 1 || logs.entries[0].action != "block" {
		t.Errorf("expected one block log, got %+v", logs.entries)
	}
}

func TestServer_AllowsUnmatched(t *testing.T) {
	fwd := &fakeForwarder{}
	logs := &fakeLogs{}
	_, addr := startServer(t, nil, fwd, nil, logs)

	resp := query(t, addr, "good.com", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("expected NOERROR, got %s", dns.RcodeToString[resp.Rcode])
	}
	if fwd.calls != 1 {
		t.Errorf("expected 1 forward call, got %d", fwd.calls)
	}
	if len(logs.entries) != 0 {
		t.Errorf("plain allow should not log, got %+v", logs.entries)
	}
}

func TestServer_RouteInstallsHostRoutes(t *testing.T) {
	answer := new(dns.Msg)
	answer.Answer = []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "x.work.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.ParseIP("1.2.3.4"),
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "x.work.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.ParseIP("5.6.7.8"),
		},
	}
	fwd := &fakeForwarder{resp: answer}
	routes := &fakeRoutes{}
	logs := &fakeLogs{}
	rs := []rules.Rule{
		{ID: 7, Pattern: "*.work.com", Action: rules.ActionAllow, Interface: "utun3", Enabled: true},
	}
	_, addr := startServer(t, rs, fwd, routes, logs)

	resp := query(t, addr, "x.work.com", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got %s", dns.RcodeToString[resp.Rcode])
	}
	routes.mu.Lock()
	calls := append([]routeCall(nil), routes.calls...)
	routes.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 route installs, got %d (%+v)", len(calls), calls)
	}
	for _, c := range calls {
		if c.iface != "utun3" || c.ruleID != 7 {
			t.Errorf("bad route call: %+v", c)
		}
	}
	if len(logs.entries) != 1 || logs.entries[0].action != "route" || logs.entries[0].iface != "utun3" {
		t.Errorf("expected one route log, got %+v", logs.entries)
	}
}

type fakeIfaces struct{ up map[string]bool }

func (f fakeIfaces) IsUp(name string) bool { return f.up[name] }

func TestServer_RouteIfaceDown_NXDOMAIN(t *testing.T) {
	answer := new(dns.Msg)
	answer.Answer = []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "x.work.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.ParseIP("1.2.3.4"),
		},
	}
	fwd := &fakeForwarder{resp: answer}
	routes := &fakeRoutes{}
	logs := &fakeLogs{}
	rs := []rules.Rule{
		{ID: 9, Pattern: "*.work.com", Action: rules.ActionAllow, Interface: "utun3", Enabled: true},
	}

	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	eng := decision.New(ruleSet(rs))
	if err := eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		Listen:     addr,
		Decider:    eng,
		Forwarder:  fwd,
		Routes:     routes,
		Interfaces: fakeIfaces{up: map[string]bool{"utun3": false}},
		Logs:       logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Start(ctx) }()
	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("not ready")
	}

	resp := query(t, addr, "x.work.com", dns.TypeA)
	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN with iface down, got %s", dns.RcodeToString[resp.Rcode])
	}
	if fwd.calls != 0 {
		t.Errorf("forwarder should not be called when iface is down, got %d", fwd.calls)
	}
	if len(routes.calls) != 0 {
		t.Errorf("no routes should be installed when iface is down, got %d", len(routes.calls))
	}
	if len(logs.entries) != 1 || logs.entries[0].action != "block-iface-down" {
		t.Errorf("expected one block-iface-down log, got %+v", logs.entries)
	}
}

func TestServer_AllowOverridesBlock(t *testing.T) {
	fwd := &fakeForwarder{}
	logs := &fakeLogs{}
	rs := []rules.Rule{
		{ID: 1, Pattern: "*.y.com", Action: rules.ActionBlock, Enabled: true},
		{ID: 2, Pattern: "safe.y.com", Action: rules.ActionAllow, Enabled: true},
	}
	_, addr := startServer(t, rs, fwd, nil, logs)

	resp := query(t, addr, "safe.y.com", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("expected NOERROR for explicit allow, got %s", dns.RcodeToString[resp.Rcode])
	}
	if fwd.calls != 1 {
		t.Errorf("explicit allow should forward")
	}

	resp = query(t, addr, "other.y.com", dns.TypeA)
	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN for sibling, got %s", dns.RcodeToString[resp.Rcode])
	}
}

func TestServer_ServFailOnUpstreamError(t *testing.T) {
	fwd := &fakeForwarder{err: errors.New("boom")}
	_, addr := startServer(t, nil, fwd, nil, nil)
	resp := query(t, addr, "anything.com", dns.TypeA)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("expected SERVFAIL on upstream error, got %s", dns.RcodeToString[resp.Rcode])
	}
}
