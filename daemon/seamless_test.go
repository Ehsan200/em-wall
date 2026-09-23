package main

import (
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

func TestDiverseOrder(t *testing.T) {
	route := map[string]string{"a": "X", "b": "X", "c": "Y", "d": "Z", "e": "Y"}
	got := diverseOrder([]string{"a", "b", "c", "d", "e"}, func(n string) string { return route[n] })
	if want := []string{"a", "c", "d", "b", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	two := []string{"a", "b"}
	if got := diverseOrder(two, func(string) string { return "same" }); !reflect.DeepEqual(got, two) {
		t.Fatalf("two-member binding reordered: %v", got)
	}
}

func TestXrayRouteKeys(t *testing.T) {
	vm := func(host string, port int) string {
		return `{"protocol":"vmess","settings":{"vnext":[{"address":"` + host + `","port":` + strconv.Itoa(port) + `}]}}`
	}
	keys := xrayRouteKeys([]xray.Config{
		{Name: "nyc", Outbound: vm("79.175.167.62", 11809)},
		{Name: "nyc-afranet-direct", Outbound: vm("79.175.167.62", 11808)},
		{Name: "nyc-direct", Outbound: vm("157.245.252.237", 11800)},
		{Name: "nyc-direct-nap", Dialer: "xraysub:nap", Outbound: vm("157.245.252.237", 11800)},
		{Name: "nyc-mouz-shop-nap", Dialer: "xraysub:nap", Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"172.64.155.209"}]}}`},
	})
	k := func(n string) string { return keys[xray.InternalProxyName(n)] }
	if k("nyc") != k("nyc-afranet-direct") || k("nyc") != "host:79.175.167.62" {
		t.Errorf("same relay host, different ports must share a route: %v", keys)
	}
	if k("nyc-direct-nap") != k("nyc-mouz-shop-nap") || k("nyc-direct-nap") != "pool:xraysub:nap" {
		t.Errorf("masters on one pool must share a route: %v", keys)
	}
	if k("nyc-direct") == k("nyc-direct-nap") {
		t.Errorf("direct and via-pool to the same server are different ways in: %v", keys)
	}
}

// With a stalled member first, the hedge must go to a different route: a
// sibling on the same route is likely stalled for the same reason.
func TestDialBindingHedgesToDifferentRoute(t *testing.T) {
	compressTCPTimers(t)
	proxyFirstByteTimeout = 3 * time.Second // a same-route hedge would sit through this
	compressHedge(t, 30*time.Millisecond)
	ports := map[string]int{
		"a": startStubSOCKS5TCP(t, stubTCPSilent),
		"b": startStubSOCKS5TCP(t, stubTCPSilent),
		"c": startStubSOCKS5TCP(t, stubTCPHealthy),
	}
	fakeIP := net.IPv4(198, 18, 0, 30)
	pf := tcpTestForwarder(t, fakeIP, "example.com", []string{"a", "b", "c"}, ports)
	pf.routes = &routeKeys{}
	pf.routes.set(map[string]string{"a": "route-1", "b": "route-1", "c": "route-2"})

	start := time.Now()
	_, used, _, err := dialThrough(t, pf, fakeIP, clientHello)
	if err != nil || used != "c" {
		t.Fatalf("used %q, err %v; want c", used, err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %s — hedged onto the stalled route first", el)
	}
}

func TestSiteKey(t *testing.T) {
	cases := map[string]string{
		"rr1---sn-t0a7lnee.googlevideo.com": "site:googlevideo.com",
		"www.youtube.com.":                  "site:youtube.com",
		"youtube.com":                       "",
		"a.b.example.co.uk":                 "site:example.co.uk",
		"example.co.uk":                     "",
		"142.251.14.138":                    "",
		"":                                  "",
	}
	for in, want := range cases {
		if got := siteKey(in); got != want {
			t.Errorf("siteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A new host with no history starts from the member its site last used.
func TestIncumbentFallsBackToSite(t *testing.T) {
	pf := &proxyForwarder{sticky: newStickyBindings()}
	pf.sticky.Set(siteKey("rr1---sn-a.googlevideo.com"), "good")
	if got := pf.incumbent(proxy.Entry{Hostname: "rr5---sn-b.googlevideo.com"}); got != "good" {
		t.Fatalf("incumbent = %q, want the site's member", got)
	}
	pf.sticky.Set("rr5---sn-b.googlevideo.com", "own")
	if got := pf.incumbent(proxy.Entry{Hostname: "rr5---sn-b.googlevideo.com"}); got != "own" {
		t.Fatalf("incumbent = %q, want the host's own binding first", got)
	}
	// A member that fails for a host is also taken off the site.
	pf.noteUpstreamFailure(proxy.Entry{Hostname: "rr5---sn-b.googlevideo.com"}, "good")
	if got := pf.sticky.Get(siteKey("x.googlevideo.com")); got != "" {
		t.Fatalf("failing member still offered to siblings: %q", got)
	}
}

func TestNetResetterOnlyOnRealChange(t *testing.T) {
	route := "en0|192.168.1.1|svc"
	var reasons []string
	r := &netResetter{read: func() string { return route }, reset: func(why string) { reasons = append(reasons, why) }}
	r.route = r.read()

	r.networkEvent() // DNS churn: same route
	if len(reasons) != 0 {
		t.Fatalf("reset on an unchanged route: %v", reasons)
	}
	route = "en0|10.0.0.1|svc2" // switched Wi-Fi
	r.networkEvent()
	r.networkEvent()
	if len(reasons) != 1 {
		t.Fatalf("resets = %v, want exactly one", reasons)
	}
}

func TestParsePrimaryRoute(t *testing.T) {
	out := "<dictionary> {\n  PrimaryInterface : en0\n  PrimaryService : 66BD\n  Router : 192.16.95.1\n}\n"
	if got := parsePrimaryRoute(out); got != "en0|192.16.95.1|66BD" {
		t.Fatalf("got %q", got)
	}
	if parsePrimaryRoute("  No such key\n") != "" {
		t.Fatalf("no network must read as empty")
	}
}

func TestSleptGap(t *testing.T) {
	if sleptGap(10*time.Second, 10*time.Second) {
		t.Fatalf("normal tick read as sleep")
	}
	if !sleptGap(20*time.Minute, 10*time.Second) {
		t.Fatalf("20 min of wall time over a 10 s tick is a sleep")
	}
}
