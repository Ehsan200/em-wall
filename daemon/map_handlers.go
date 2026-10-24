package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
	"github.com/miekg/dns"
)

// registerMapHandlers registers the map.* IPC methods. Contract: docs/geo-map-suggest.md.
func registerMapHandlers(s *ipc.Server, d *handlerDeps) {
	svc := newMapService(d)
	s.Handle(ipc.MethodMapPaths, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.MapPathsParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
		}
		return svc.paths(ctx, p), nil
	})
}

// mapExits is the exit-key registry the proxy tunnel measures, set from
// main.go with setMapExitKeys. Unset, exit hops come only from the
// resolveExitIP cache.
var mapExits atomic.Pointer[exitKeys]

func setMapExitKeys(e *exitKeys) { mapExits.Store(e) }

// Cache lifetimes for what map.paths reads from the stores. The map is
// polled every 2s; rules, entries and node names change on user action or
// a subscription refresh, and a few seconds' lag on the map costs nothing.
const (
	mapRulesTTL     = 15 * time.Second
	mapUpstreamsTTL = 30 * time.Second
	mapNodeNamesTTL = time.Minute
	// mapPublicIPKick spaces background refreshes of the public IP when
	// the net.public-ip cache is cold.
	mapPublicIPKick = time.Minute
)

// mapService is map.paths' daemon side: the caches that let a poll answer
// from memory.
type mapService struct {
	d    *handlerDeps
	now  func() time.Time
	res  *hostResolver // server hosts, via the daemon's resolvers
	dest *hostResolver // destinations, via the flow's upstream
	// dialVia dials through a proxy-store name; tests replace it.
	dialVia func(ctx context.Context, name string) (dialContext, error)

	mu        sync.Mutex
	rules     map[int64]mapRule
	rulesAt   time.Time
	entries   map[string]xray.Config // hidden proxy name → entry
	entriesAt time.Time
	names     map[string]string // pool member key → display name
	namesAt   time.Time
	publicAt  time.Time // last background public-IP refresh
}

func newMapService(d *handlerDeps) *mapService {
	m := &mapService{d: d, now: time.Now}
	m.dialVia = m.dialViaUpstream
	m.res = newHostResolver(m.lookupHost, d.bgContext)
	m.dest = newHostResolver(m.lookupDest, d.bgContext)
	return m
}

func (m *mapService) paths(ctx context.Context, p ipc.MapPathsParams) ipc.MapPathsResult {
	now := m.now()
	m.ensureCityData()
	var live *liveConns
	if m.d.xraySup != nil {
		live = m.d.xraySup.live
	}
	live.watch(now)
	env := m.env(ctx, now)
	var loc geo.Locator = noLocator{}
	if m.d.geoDB != nil {
		loc = m.d.geoDB
	}
	return ipc.MapPathsResult{
		Flows:       buildMapFlows(live.flows(now), env, loc, p),
		CityData:    m.hasCityData(),
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}
}

// ensureCityData asks the geo updater to fetch the DB-IP files when they
// are missing (spec: fetched on first map.paths, not at install).
func (m *mapService) ensureCityData() { m.d.ensureCityData() }

// hasCityData reports whether city precision is available: the City mmdb
// is loaded in the current geo snapshot.
func (m *mapService) hasCityData() bool {
	return m.d.geoDB != nil && m.d.geoDB.Stats().CityLoaded
}

// env refreshes whatever cache is stale and returns one poll's consistent
// view.
func (m *mapService) env(ctx context.Context, now time.Time) *mapCallEnv {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rules == nil || now.Sub(m.rulesAt) >= mapRulesTTL {
		if r, err := m.loadRules(ctx); err == nil {
			m.rules, m.rulesAt = r, now
		}
	}
	if m.entries == nil || now.Sub(m.entriesAt) >= mapUpstreamsTTL {
		if m.d.xrayStore != nil {
			if list, err := m.d.xrayStore.List(ctx); err == nil {
				es := make(map[string]xray.Config, len(list))
				for _, e := range list {
					es[xray.InternalProxyName(e.Name)] = e
				}
				m.entries, m.entriesAt = es, now
			}
		}
	}
	if m.names == nil || now.Sub(m.namesAt) >= mapNodeNamesTTL {
		m.names, m.namesAt = poolNodeNamesFrom(ctx, m.d.xrayStore), now
	}
	env := &mapCallEnv{
		ctx: ctx, svc: m, rules: m.rules, entries: m.entries, names: m.names,
		exits: mapExits.Load().snapshot(),
	}
	if sup := m.d.xraySup; sup != nil {
		sup.mu.Lock()
		env.slots = sup.loadedSlots
		sup.mu.Unlock()
		env.timeline = sup.timeline
	}
	env.public = m.publicIPLocked(now)
	return env
}

// publicIPLocked reads the net.public-ip cache, refreshing it in the
// background (at most once per mapPublicIPKick) when it is cold. The probe
// is the same one the top bar already runs — nothing new is sent.
func (m *mapService) publicIPLocked(now time.Time) string {
	if r, ok := cacheExitGet(""); ok {
		return r.ExitIP
	}
	if now.Sub(m.publicAt) >= mapPublicIPKick {
		m.publicAt = now
		d := m.d
		go func() {
			ctx, cancel := context.WithTimeout(d.bgContext(), 15*time.Second)
			defer cancel()
			d.resolveExitIP(ctx, "")
		}()
	}
	return ""
}

// loadRules attributes every stored rule to its group: curated registry
// first (in its order, as groups.GroupForKey does), then custom groups.
func (m *mapService) loadRules(ctx context.Context) (map[int64]mapRule, error) {
	if m.d.store == nil {
		return map[int64]mapRule{}, nil
	}
	rs, err := m.d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var defs []mapGroupDef
	for _, g := range groups.KnownGroups() {
		defs = append(defs, mapGroupDef{
			mapGroup: mapGroup{key: g.Key, name: g.DisplayName, color: groups.BrandColor(g.Key)},
			patterns: m.d.effectiveGroupPatterns(ctx, g),
		})
	}
	if custom, err := m.d.store.ListCustomGroups(ctx); err == nil {
		for _, g := range custom {
			defs = append(defs, mapGroupDef{
				mapGroup: mapGroup{key: g.Key, name: g.DisplayName, color: g.Color},
				patterns: g.Patterns,
			})
		}
	}
	return attributeRules(rs, defs), nil
}

// lookupHost resolves a server host through the daemon's own upstream
// resolvers — the system resolver would be us. Only for hosts this machine
// dials itself (entry servers, pool nodes); never for destinations.
func (m *mapService) lookupHost(ctx context.Context, host, _ string) (netip.Addr, error) {
	m.d.mu.Lock()
	servers := splitCSV(m.d.upstream)
	m.d.mu.Unlock()
	if len(servers) == 0 {
		servers = publicFallbackDNS
	}
	fwd := dnsproxy.NewMultiUpstream(servers, 2*time.Second)
	return lookupAddr(func(q *dns.Msg) (*dns.Msg, error) { return fwd.Forward(ctx, q) }, host)
}

// mapDestResolvers are the public resolvers a destination is looked up at,
// over TCP through the flow's own upstream.
var mapDestResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// lookupDest resolves a FakeIP'd destination through via, the upstream
// carrying the flow: DNS over TCP to a public resolver, dialed through
// that upstream like probeExitIPVia does. An upstream that can't be dialed
// leaves the destination unresolved — there is no local fallback, since
// the point of proxying the name was that the local network never sees it.
// The resolver answering from the exit's side also places the destination
// where the exit, not this machine, reaches it.
func (m *mapService) lookupDest(ctx context.Context, host, via string) (netip.Addr, error) {
	dial, err := m.dialVia(ctx, via)
	if err != nil {
		return netip.Addr{}, err
	}
	var lastErr error = errors.New("no address")
	for _, srv := range mapDestResolvers {
		c, err := dial(ctx, "tcp", srv)
		if err != nil {
			lastErr = err
			continue
		}
		a, err := func() (netip.Addr, error) {
			defer c.Close()
			if dl, ok := ctx.Deadline(); ok {
				_ = c.SetDeadline(dl)
			}
			dc := &dns.Conn{Conn: c}
			return lookupAddr(func(q *dns.Msg) (*dns.Msg, error) {
				if err := dc.WriteMsg(q); err != nil {
					return nil, err
				}
				return dc.ReadMsg()
			}, host)
		}()
		if err == nil {
			return a, nil
		}
		lastErr = err
	}
	return netip.Addr{}, lastErr
}

// dialViaUpstream returns a dialer through the proxy-store entry name (an
// xray entry's hidden row or a plain proxy).
func (m *mapService) dialViaUpstream(ctx context.Context, name string) (dialContext, error) {
	if m.d.proxyStore == nil || name == "" {
		return nil, errors.New("no upstream")
	}
	p, err := m.d.proxyStore.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	d, err := proxy.NewDialer(p)
	if err != nil {
		return nil, err
	}
	return proxyDialContext(d), nil
}

// lookupAddr asks exchange for host's A, then AAAA, record; first wins.
func lookupAddr(exchange func(*dns.Msg) (*dns.Msg, error), host string) (netip.Addr, error) {
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(host), qt)
		resp, err := exchange(q)
		if err != nil || resp == nil {
			continue
		}
		for _, rr := range resp.Answer {
			switch v := rr.(type) {
			case *dns.A:
				if a, ok := netip.AddrFromSlice(v.A); ok {
					return a.Unmap(), nil
				}
			case *dns.AAAA:
				if a, ok := netip.AddrFromSlice(v.AAAA); ok {
					return a, nil
				}
			}
		}
	}
	return netip.Addr{}, errors.New("no address")
}

// mapCallEnv is mapEnv over one poll's snapshot of the caches.
type mapCallEnv struct {
	ctx      context.Context
	svc      *mapService
	rules    map[int64]mapRule
	entries  map[string]xray.Config
	names    map[string]string
	exits    map[string]string
	slots    []xray.DialerSlot
	timeline *poolTimeline
	public   string
}

func (e *mapCallEnv) rule(id int64) (mapRule, bool) {
	r, ok := e.rules[id]
	return r, ok
}

func (e *mapCallEnv) upstream(name string) mapUpstream {
	if ent, ok := e.entries[name]; ok {
		return mapUpstream{
			display:    ent.Name,
			serverHost: xray.OutboundServerHost(ent.Outbound),
			master:     strings.TrimSpace(ent.Dialer) != "",
		}
	}
	u := mapUpstream{display: displayUpstream(name)}
	if !xray.IsInternalProxyName(name) && e.svc.d.proxyStore != nil {
		// Served from the store's in-memory cache.
		if p, err := e.svc.d.proxyStore.GetByName(e.ctx, name); err == nil {
			u.serverHost = p.Host
		}
	}
	return u
}

func (e *mapCallEnv) poolCarriers(name string) []mapPoolNode {
	ent, ok := e.entries[name]
	if !ok || strings.TrimSpace(ent.Dialer) == "" {
		return nil
	}
	keys, ok := e.timeline.carriers(ent.Name)
	if !ok || len(keys) == 0 {
		return nil
	}
	hosts := map[string]string{}
	for _, sl := range e.slots {
		if !strings.EqualFold(sl.Master, ent.Name) && !containsFold(sl.Aliases, ent.Name) {
			continue
		}
		for _, mb := range sl.Members {
			hosts[mb.Key] = xray.OutboundServerHost(string(mb.Outbound))
		}
	}
	out := make([]mapPoolNode, 0, len(keys))
	for _, k := range keys {
		label := e.names[k]
		if label == "" {
			label = memberDisplayName(k)
		}
		out = append(out, mapPoolNode{key: k, label: label, host: hosts[k]})
	}
	return out
}

func (e *mapCallEnv) exitIP(name string) string {
	if ip := e.exits[name]; ip != "" {
		return ip
	}
	// A single-member binding isn't in exitKeys; the UI's own exit lookup
	// may have measured it.
	iface := "proxy:" + name
	if ent, ok := e.entries[name]; ok {
		iface = "xray:" + ent.Name
	}
	if r, ok := cacheExitGet(iface); ok && r.Probed {
		return r.ExitIP
	}
	return ""
}

func (e *mapCallEnv) publicIP() string { return e.public }

func (e *mapCallEnv) resolve(host string) (netip.Addr, bool) { return e.svc.res.get(host, "") }

func (e *mapCallEnv) resolveDest(host, via string) (netip.Addr, bool) {
	return e.svc.dest.get(host, via)
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
