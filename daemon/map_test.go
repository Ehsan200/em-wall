package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
	"github.com/miekg/dns"
)

type fakeLocator map[netip.Addr]geo.Location

func (f fakeLocator) Locate(a netip.Addr) (geo.Location, bool) {
	l, ok := f[a]
	return l, ok
}

type fakeMapEnv struct {
	rules    map[int64]mapRule
	ups      map[string]mapUpstream
	carriers map[string][]mapPoolNode
	exits    map[string]string
	public   string
	hosts    map[string]netip.Addr
	resolved []string
	destVia  map[string]string // dest host → upstream it was resolved through
}

func (f *fakeMapEnv) rule(id int64) (mapRule, bool) { r, ok := f.rules[id]; return r, ok }
func (f *fakeMapEnv) upstream(n string) mapUpstream {
	if u, ok := f.ups[n]; ok {
		return u
	}
	return mapUpstream{display: n}
}
func (f *fakeMapEnv) poolCarriers(n string) []mapPoolNode { return f.carriers[n] }
func (f *fakeMapEnv) exitIP(n string) string              { return f.exits[n] }
func (f *fakeMapEnv) publicIP() string                    { return f.public }
func (f *fakeMapEnv) resolveDest(h, via string) (netip.Addr, bool) {
	if f.destVia == nil {
		f.destVia = map[string]string{}
	}
	f.destVia[h] = via
	a, ok := f.hosts[h]
	return a, ok
}
func (f *fakeMapEnv) resolve(h string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(h); err == nil {
		return a, true
	}
	f.resolved = append(f.resolved, h)
	a, ok := f.hosts[h]
	return a, ok
}

var (
	ipYou    = netip.MustParseAddr("5.1.1.1")
	ipNode   = netip.MustParseAddr("6.1.1.1")
	ipServer = netip.MustParseAddr("7.1.1.1")
	ipExit   = netip.MustParseAddr("8.1.1.1")
	ipAms    = netip.MustParseAddr("9.1.1.1")
	ipAms2   = netip.MustParseAddr("9.1.1.2")
	ipNyc    = netip.MustParseAddr("9.2.2.2")
	ipFake   = netip.MustParseAddr("198.18.0.7")
)

func city(cc, name string, lat, lon float64) geo.Location {
	return geo.Location{Country: cc, City: name, Lat: lat, Lon: lon, Precision: geo.PrecisionCity}
}

func testLocator() fakeLocator {
	return fakeLocator{
		ipYou:    city("IR", "Tehran", 35.7, 51.4),
		ipNode:   city("TR", "Istanbul", 41, 29),
		ipServer: city("DE", "Frankfurt", 50.1, 8.7),
		ipExit:   city("NL", "Amsterdam", 52.4, 4.9),
		ipAms:    city("NL", "Amsterdam", 52.4, 4.9),
		ipAms2:   city("NL", "Amsterdam", 52.4, 4.9),
		ipNyc:    city("US", "New York", 40.7, -74),
	}
}

func TestAttributeRules(t *testing.T) {
	rs := []rules.Rule{
		{ID: 1, Pattern: "*.spotify.com", Interface: "xrayset:nj"},
		{ID: 2, Pattern: "api.spotify.com", Interface: "xray:a"},
		{ID: 3, Pattern: "*.example.org", Interface: "xray:b"},
		{ID: 4, Pattern: "*.custom.io", Interface: "proxy:p"},
		{ID: 5, Pattern: "*.com", Interface: "xray:c"}, // wider than any group
	}
	defs := []mapGroupDef{
		{mapGroup: mapGroup{key: "spotify", name: "Spotify", color: "#1db954"}, patterns: []string{"*.spotify.com", "*.scdn.co"}},
		{mapGroup: mapGroup{key: "custom:x", name: "Mine"}, patterns: []string{"*.spotify.com", "*.custom.io"}},
	}
	got := attributeRules(rs, defs)
	want := map[int64]string{1: "spotify", 2: "spotify", 3: "", 4: "custom:x", 5: ""}
	for id, key := range want {
		if got[id].group.key != key {
			t.Errorf("rule %d: group %q, want %q", id, got[id].group.key, key)
		}
	}
	if got[1].iface != "xrayset:nj" || got[1].group.color != "#1db954" {
		t.Errorf("rule 1 = %+v", got[1])
	}
}

func mapFixture() *fakeMapEnv {
	return &fakeMapEnv{
		rules: map[int64]mapRule{
			1: {iface: "xrayset:nj", group: mapGroup{key: "spotify", name: "Spotify", color: "#1db954"}},
			2: {iface: "xray:solo"},
		},
		ups: map[string]mapUpstream{
			"_xray_m":    {display: "m", serverHost: "srv.example", master: true},
			"_xray_solo": {display: "solo", serverHost: ipServer.String()},
		},
		carriers: map[string][]mapPoolNode{
			"_xray_m": {
				{key: "n1", label: "sub/Istanbul", host: "node1.example"},
				{key: "n2", label: "sub/Berlin"},
				{key: "n3", label: "sub/Paris"},
			},
		},
		exits:  map[string]string{"_xray_m": ipExit.String(), "_xray_solo": ipServer.String()},
		public: ipYou.String(),
		hosts: map[string]netip.Addr{
			"node1.example": ipNode, "srv.example": ipServer,
			"a.spotify.com": ipAms, "b.spotify.com": ipAms2, "c.spotify.com": ipNyc,
		},
	}
}

// Connections on one path into one city fold into one flow; another city
// is another flow. Rates and bytes sum, hosts are listed busiest first.
func TestBuildMapFlowsAggregates(t *testing.T) {
	env := mapFixture()
	conns := []liveFlow{
		{name: "_xray_m", flow: connFlow{Host: "a.spotify.com", DestIP: ipFake, RuleID: 1}, up: 10, down: 100, rateUp: 1, rateDown: 10},
		{name: "_xray_m", flow: connFlow{Host: "b.spotify.com", DestIP: ipFake, RuleID: 1}, up: 20, down: 900, rateUp: 2, rateDown: 20},
		{name: "_xray_m", flow: connFlow{Host: "c.spotify.com", DestIP: ipFake, RuleID: 1}, up: 1, down: 1},
		{name: "_xray_solo", flow: connFlow{Host: ipNyc.String(), DestIP: ipNyc, RuleID: 2}, up: 5, down: 5, rateDown: 100},
	}
	flows := buildMapFlows(conns, env, testLocator(), ipc.MapPathsParams{})
	if len(flows) != 3 {
		t.Fatalf("got %d flows, want 3: %+v", len(flows), flows)
	}
	// Busiest (rate) first: solo (100), then Amsterdam (33), then New York.
	solo, ams, nyc := flows[0], flows[1], flows[2]
	if solo.Upstream != "solo" || ams.Upstream != "m" || nyc.Hops[len(nyc.Hops)-1].City != "New York" {
		t.Fatalf("order: %s %s %s", solo.Upstream, ams.Upstream, nyc.Upstream)
	}
	if ams.Conns != 2 || ams.RateUp != 3 || ams.RateDown != 30 || ams.BytesUp != 30 || ams.BytesDown != 1000 {
		t.Errorf("amsterdam flow = %+v", ams)
	}
	if !slices.Equal(ams.Hosts, []string{"b.spotify.com", "a.spotify.com"}) {
		t.Errorf("hosts = %v", ams.Hosts)
	}
	if ams.Group != "spotify" || ams.GroupColor != "#1db954" || ams.Iface != "xrayset:nj" {
		t.Errorf("group/iface = %q %q %q", ams.Group, ams.GroupColor, ams.Iface)
	}
	if env.destVia["a.spotify.com"] != "_xray_m" {
		t.Errorf("dest resolved via %q, want the flow's upstream", env.destVia["a.spotify.com"])
	}
	dest := ams.Hops[len(ams.Hops)-1]
	if dest.Role != ipc.MapHopDest || dest.IP != ipAms2.String() || dest.Host != "b.spotify.com" || dest.City != "Amsterdam" {
		t.Errorf("dest hop = %+v (want the busiest connection's)", dest)
	}

	// Filters.
	if got := buildMapFlows(conns, env, testLocator(), ipc.MapPathsParams{Group: "spotify"}); len(got) != 2 {
		t.Errorf("group filter: %d flows", len(got))
	}
	if got := buildMapFlows(conns, env, testLocator(), ipc.MapPathsParams{Iface: "xray:solo"}); len(got) != 1 || got[0].Upstream != "solo" {
		t.Errorf("iface filter: %+v", got)
	}
}

func roles(hops []ipc.MapHopDTO) []string {
	out := make([]string, len(hops))
	for i, h := range hops {
		out[i] = h.Role
	}
	return out
}

func TestMapHops(t *testing.T) {
	env := mapFixture()
	loc := testLocator()
	conns := []liveFlow{
		{name: "_xray_m", flow: connFlow{Host: "a.spotify.com", DestIP: ipFake, RuleID: 1}},
		{name: "_xray_solo", flow: connFlow{Host: ipNyc.String(), DestIP: ipNyc, RuleID: 2}, up: 1},
	}
	flows := buildMapFlows(conns, env, loc, ipc.MapPathsParams{})
	byUp := map[string]ipc.MapFlowDTO{}
	for _, f := range flows {
		byUp[f.Upstream] = f
	}

	m := byUp["m"]
	if want := []string{"you", "pool", "server", "exit", "dest"}; !slices.Equal(roles(m.Hops), want) {
		t.Fatalf("master hops %v, want %v", roles(m.Hops), want)
	}
	you, pool, server, exit := m.Hops[0], m.Hops[1], m.Hops[2], m.Hops[3]
	if you.IP != ipYou.String() || you.City != "Tehran" {
		t.Errorf("you = %+v", you)
	}
	if !pool.Uncertain || pool.Label != "sub/Istanbul" || pool.City != "Istanbul" ||
		!slices.Equal(pool.Alternatives, []string{"sub/Berlin", "sub/Paris"}) {
		t.Errorf("pool = %+v", pool)
	}
	if server.Label != "m" || server.Host != "srv.example" || server.IP != ipServer.String() || server.City != "Frankfurt" {
		t.Errorf("server = %+v", server)
	}
	if exit.IP != ipExit.String() || exit.Country != "NL" {
		t.Errorf("exit = %+v", exit)
	}

	// Not a master: no pool hop; its exit is its server, so no exit hop.
	// A real (non-FakeIP) destination is used as is, never resolved.
	s := byUp["solo"]
	if want := []string{"you", "server", "dest"}; !slices.Equal(roles(s.Hops), want) {
		t.Errorf("solo hops %v, want %v", roles(s.Hops), want)
	}
	if d := s.Hops[2]; d.IP != ipNyc.String() || d.City != "New York" {
		t.Errorf("solo dest = %+v", d)
	}
	if slices.Contains(env.resolved, ipNyc.String()) {
		t.Error("a real destination IP went through resolve")
	}
}

// Unknown public IP, unresolved names and an unlocatable server still
// produce a flow: the hops are there, just unplaced.
func TestMapHopsUnplaced(t *testing.T) {
	env := mapFixture()
	env.public = ""
	env.hosts = nil
	flows := buildMapFlows([]liveFlow{
		{name: "_xray_m", flow: connFlow{Host: "a.spotify.com", DestIP: ipFake, RuleID: 1}},
	}, env, fakeLocator{}, ipc.MapPathsParams{})
	if len(flows) != 1 {
		t.Fatalf("flows = %+v", flows)
	}
	for _, h := range flows[0].Hops {
		if h.Precision != "" || (h.IP != "" && h.Role != ipc.MapHopExit) {
			t.Errorf("hop %s should be unplaced: %+v", h.Role, h)
		}
	}
	if d := flows[0].Hops[len(flows[0].Hops)-1]; d.Host != "a.spotify.com" {
		t.Errorf("dest = %+v", d)
	}
}

func TestRateWindow(t *testing.T) {
	var w rateWindow
	if _, _, ok := w.rates(rateSample{at: int64(time.Second)}); ok {
		t.Fatal("empty window gave a rate")
	}
	sec := int64(time.Second)
	for i := int64(0); i < 8; i++ { // ring keeps the last rateSlots
		w.add(rateSample{at: i * sec, up: i * 100, down: i * 1000})
	}
	up, down, ok := w.rates(rateSample{at: 8 * sec, up: 800, down: 8000})
	if !ok || up != 100 || down != 1000 {
		t.Errorf("rates = %v %v %v, want 100 1000", up, down, ok)
	}
	// A gap (sampling was off) restarts the ring instead of averaging over it.
	w.add(rateSample{at: 60 * sec, up: 800, down: 8000})
	up, _, _ = w.rates(rateSample{at: 61 * sec, up: 1800, down: 8000})
	if up != 1000 {
		t.Errorf("after gap up = %v, want 1000", up)
	}
}

// Counting, the watch gate and the lifetime-average fallback, through real
// connections.
func TestLiveFlowsCountAndRate(t *testing.T) {
	l := newLiveConns(nil)
	up, far := tcpPair(t)
	client, _ := tcpPair(t)
	tr, untrack := l.trackFlow("_xray_m", connFlow{Host: "x.example", RuleID: 7, Sent: 50}, client, up)
	defer untrack()

	if _, err := tr.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	_, _ = io.ReadFull(far, buf)
	_, _ = far.Write(make([]byte, 300))
	_, _ = io.ReadFull(tr, make([]byte, 300))

	now := time.Now()
	l.sampleRates(now) // not watched: no samples
	f := l.flows(now.Add(time.Second))
	if len(f) != 1 || f[0].up != 150 || f[0].down != 300 || f[0].flow.RuleID != 7 {
		t.Fatalf("flows = %+v", f)
	}
	if f[0].rateDown <= 0 {
		t.Errorf("no lifetime-average rate before sampling: %+v", f[0])
	}
	var c *trackedConn
	for k := range l.conns {
		c = k
	}
	if c.rate.n != 0 {
		t.Fatal("sampled while unwatched")
	}
	l.watch(now)
	l.sampleRates(now)
	_, _ = far.Write(make([]byte, 1000))
	_, _ = io.ReadFull(tr, make([]byte, 1000))
	f = l.flows(now.Add(2 * time.Second))
	if f[0].rateDown != 500 {
		t.Errorf("rateDown = %v, want 500", f[0].rateDown)
	}
	if l.sampleRates(now.Add(mapWatchTTL + time.Second)); c.rate.n != 1 {
		t.Errorf("sampled past the watch: n=%d", c.rate.n)
	}
}

// The splice path, the sampler and the map poll run concurrently; -race
// checks they share nothing unsynchronised.
func TestLiveFlowsNoRace(t *testing.T) {
	l := newLiveConns(nil)
	l.watch(time.Now())
	up, far := tcpPair(t)
	client, _ := tcpPair(t)
	tr, untrack := l.trackFlow("_xray_m", connFlow{Host: "x"}, client, up)
	defer untrack()
	go func() { _, _ = io.Copy(io.Discard, far) }()
	go func() { _, _ = io.Copy(io.Discard, tr) }()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = tr.Write([]byte("abc"))
				_, _ = far.Write([]byte("xyz"))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				l.sampleRates(time.Now())
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = l.flows(time.Now())
				_, u := l.track("_xray_n", client, up)
				u()
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestHostResolver(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	r := newHostResolver(func(ctx context.Context, host, _ string) (netip.Addr, error) {
		calls.Add(1)
		<-release
		if host == "bad.example" {
			return netip.Addr{}, errors.New("nx")
		}
		return ipAms, nil
	}, context.Background)

	if a, ok := r.get("1.2.3.4", ""); !ok || a.String() != "1.2.3.4" {
		t.Fatal("literal not passed through")
	}
	if _, ok := r.get("Good.Example.", ""); ok {
		t.Fatal("miss answered synchronously")
	}
	if _, ok := r.get("good.example", ""); ok { // pending: no second lookup
		t.Fatal("pending answered")
	}
	_, _ = r.get("bad.example", "")
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		a, ok := r.get("good.example", "")
		if ok {
			if a != ipAms {
				t.Fatalf("got %v", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lookup never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := r.get("bad.example", ""); ok {
		t.Error("failed name answered")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("lookups = %d, want 2 (failure cached)", n)
	}
}

func TestTimelineCarriers(t *testing.T) {
	tl := newPoolTimeline()
	tl.pools["m"] = &poolHistory{
		masters: []string{"m", "m2"},
		samples: []poolSample{
			{nodes: map[string]nodeCell{"a": {role: ipc.PoolRoleFallback}, "b": {role: ipc.PoolRoleActive, down: 5000}, "c": {role: ipc.PoolRoleIdle}}},
			{nodes: map[string]nodeCell{"a": {role: ipc.PoolRoleFallback, down: 100}, "b": {role: ipc.PoolRoleActive}, "c": {role: ipc.PoolRoleIdle}, "d": {role: ipc.PoolRoleActive}}},
		},
	}
	keys, ok := tl.carriers("M2") // an alias, case-insensitively
	if !ok || !slices.Equal(keys, []string{"b", "a", "d"}) {
		t.Errorf("carriers = %v %v, want [b a d]", keys, ok)
	}
	if _, ok := tl.carriers("other"); ok {
		t.Error("unknown master reported a pool")
	}
	var nilTL *poolTimeline
	if _, ok := nilTL.carriers("m"); ok {
		t.Error("nil timeline")
	}
}

// The access log's picks decide the pool hop when it has them: one node
// taking nearly every recent pick is certain, a split lists the others by
// share; with no picks the byte-ranked timeline carriers are the guess.
func TestMapPoolHopFromPicks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := newXrayAccessLog("")
	a.now = func() time.Time { return now }
	slot := xray.DialerSlot{Master: "m", Aliases: []string{"m2"}, Index: 0, PoolKey: "xraysub:p", Members: []xray.DialerMember{
		{Key: "n1", Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"node1.example"}]}}`)},
		{Key: "n2"}, {Key: "n3"},
	}}
	tl := newPoolTimeline()
	tl.pools["m"] = &poolHistory{masters: []string{"m", "m2"}, samples: []poolSample{
		{nodes: map[string]nodeCell{"n3": {role: ipc.PoolRoleActive, down: 900}}},
	}}
	env := &mapCallEnv{
		entries:  map[string]xray.Config{"_xray_m2": {Name: "m2", Dialer: "xraysub:p"}},
		names:    map[string]string{"n1": "sub/Istanbul", "n2": "sub/Berlin", "n3": "sub/Paris"},
		slots:    []xray.DialerSlot{slot},
		timeline: tl,
		access:   a,
	}

	// No picks: the timeline's carriers, no shares.
	got := env.poolCarriers("_xray_m2")
	if len(got) != 1 || got[0].key != "n3" || got[0].picks != 0 {
		t.Fatalf("fallback carriers = %+v", got)
	}

	pick := func(key string, n int) {
		for i := 0; i < n; i++ {
			a.observe([]byte(accessLine("slot0-in", xray.SlotMemberTag(0, key))), now.Add(-time.Duration(i)*time.Second))
		}
	}
	pick("n1", 9)
	pick("n2", 1)
	got = env.poolCarriers("_xray_m2")
	if len(got) != 2 || got[0].key != "n1" || got[0].host != "node1.example" || got[0].share != 0.9 || got[0].picks != 9 {
		t.Fatalf("picked carriers = %+v", got)
	}
	hops := upstreamHops("_xray_m2", got, mapFixture(), testLocator())
	if pool := hops[1]; pool.Uncertain || pool.Label != "sub/Istanbul" || len(pool.Alternatives) != 0 {
		t.Errorf("dominant pool hop = %+v", pool)
	}

	pick("n2", 5) // now 9 : 6
	got = env.poolCarriers("_xray_m2")
	hops = upstreamHops("_xray_m2", got, mapFixture(), testLocator())
	if pool := hops[1]; !pool.Uncertain || !slices.Equal(pool.Alternatives, []string{"sub/Berlin (40%)"}) {
		t.Errorf("split pool hop = %+v", pool)
	}
}

// fakeDNSDial answers DNS over TCP in-process, recording what was dialed.
func fakeDNSDial(t *testing.T, dialed *[]string, answer netip.Addr) dialContext {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		*dialed = append(*dialed, network+" "+addr)
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			dc := &dns.Conn{Conn: b}
			for {
				q, err := dc.ReadMsg()
				if err != nil {
					return
				}
				r := new(dns.Msg)
				r.SetReply(q)
				if q.Question[0].Qtype == dns.TypeA {
					r.Answer = append(r.Answer, &dns.A{
						Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
						A:   answer.AsSlice(),
					})
				}
				_ = dc.WriteMsg(r)
			}
		}()
		return a, nil
	}
}

// A destination is resolved over TCP through the flow's upstream; when that
// upstream can't be dialed it stays unresolved — no local fallback.
func TestMapLookupDestViaUpstream(t *testing.T) {
	var dialed []string
	var viaAsked []string
	m := &mapService{d: &handlerDeps{upstream: "192.0.2.1:53"}}
	m.dialVia = func(ctx context.Context, name string) (dialContext, error) {
		viaAsked = append(viaAsked, name)
		if name != "_xray_m" {
			return nil, errors.New("no such upstream")
		}
		return fakeDNSDial(t, &dialed, ipAms), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, err := m.lookupDest(ctx, "a.spotify.com", "_xray_m")
	if err != nil || a != ipAms {
		t.Fatalf("lookupDest = %v %v", a, err)
	}
	if len(dialed) != 1 || dialed[0] != "tcp "+mapDestResolvers[0] {
		t.Errorf("dialed %v, want one TCP dial to %s", dialed, mapDestResolvers[0])
	}
	if _, err := m.lookupDest(ctx, "a.spotify.com", "_xray_gone"); err == nil {
		t.Error("an undialable upstream resolved anyway")
	}
	if !slices.Equal(viaAsked, []string{"_xray_m", "_xray_gone"}) {
		t.Errorf("dialVia asked %v", viaAsked)
	}

	// Through the resolver: keyed by hostname, so a second flow's upstream
	// reuses the first answer.
	m.dest = newHostResolver(m.lookupDest, context.Background)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if a, ok := m.dest.get("a.spotify.com", "_xray_m"); ok {
			if a != ipAms {
				t.Fatalf("got %v", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n := len(viaAsked)
	if a, ok := m.dest.get("a.spotify.com", "_xray_other"); !ok || a != ipAms || len(viaAsked) != n {
		t.Errorf("cache not shared across upstreams: %v %v, asked %v", a, ok, viaAsked[n:])
	}
}
