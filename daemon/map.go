package main

import (
	"context"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
)

// World map: live proxied flows, hop by hop.
//
// map.paths turns the open splices in liveConns into paths a map can draw:
// you → pool node (master entries only) → the entry's server → its measured
// exit (when that is a different machine) → the destination. Connections
// sharing a group, binding, upstream, pool node and destination city fold
// into one flow with summed rates.
//
// Observation only. Nothing here feeds a routing or selection decision, and
// nothing runs on the connection path: the splice pays two atomic adds per
// Read/Write (stalepath.go), everything else happens when the map polls.
// Every lookup that could block — resolving a server or FakeIP'd hostname,
// the public IP — is served from a cache and refreshed in the background,
// so a poll never waits on the network; an answer not in yet shows as an
// unplaced hop and fills in on a later poll.
//
// This file is the pure part (aggregation and hop construction against
// mapEnv); map_handlers.go is the daemon-side env with its caches.

const (
	mapHostsCap        = 8   // hostnames listed per flow
	mapFlowsCap        = 200 // flows per answer, busiest first
	mapAlternativesCap = 4   // other pool carriers listed on a pool hop
)

// fakeIPPrefix is the FakeIP range: a destination in it is a synthetic
// handle, never where the traffic goes.
var fakeIPPrefix = netip.MustParsePrefix(dnsproxy.DefaultFakeIPv4CIDR)

// mapGroup is the group a flow is drawn as.
type mapGroup struct{ key, name, color string }

// mapRule is what the map needs of one stored rule.
type mapRule struct {
	iface string
	group mapGroup
}

// mapUpstream describes one proxy-store name.
type mapUpstream struct {
	display    string // what the UI calls it (entry or proxy name)
	serverHost string // the server it dials, "" = unknown
	master     bool   // an xray entry with a Dialer (rides a pool)
}

// mapPoolNode is one pool member carrying a master. share and picks are
// set when the list comes from xray's access log (xray_access.go): the
// node's share of the slot's recent picks, and how many. Zero = ranked by
// the pool timeline's byte counters instead.
type mapPoolNode struct {
	key, label, host string
	share            float64
	picks            int
}

// mapEnv is everything the map reads from the rest of the daemon. Every
// method must answer from memory: it is called once per connection.
type mapEnv interface {
	rule(id int64) (mapRule, bool)
	upstream(name string) mapUpstream
	// poolCarriers returns name's pool nodes, most picked (else busiest)
	// first; nil when name is not a master or its pool has no data.
	poolCarriers(name string) []mapPoolNode
	exitIP(name string) string // measured exit IP, "" = unknown
	publicIP() string          // "" = unknown
	// resolve returns a server's address from cache (an IP literal as
	// is), kicking a background lookup on a miss; ok is false until it
	// lands. Servers are hosts this machine dials itself, so the daemon's
	// own resolvers answer them.
	resolve(host string) (netip.Addr, bool)
	// resolveDest is resolve for a FakeIP'd destination, looked up through
	// via — the upstream carrying the flow — never a local resolver: the
	// name was proxied so that nobody on this network sees it queried.
	resolveDest(host, via string) (netip.Addr, bool)
}

// mapGroupDef is a group with its patterns, for attribution.
type mapGroupDef struct {
	mapGroup
	patterns []string
}

// attributeRules maps every rule to its interface and to the first group
// (in defs order: curated registry first, then custom) whose patterns
// contain the rule's pattern — the same containment groups.list counts
// RuleCount with.
func attributeRules(rs []rules.Rule, defs []mapGroupDef) map[int64]mapRule {
	out := make(map[int64]mapRule, len(rs))
	for _, r := range rs {
		mr := mapRule{iface: r.Interface}
		for _, g := range defs {
			if ruleBelongsToGroup(r.Pattern, g.patterns) {
				mr.group = g.mapGroup
				break
			}
		}
		out[r.ID] = mr
	}
	return out
}

// flowBucket accumulates the connections of one flow.
type flowBucket struct {
	dto       ipc.MapFlowDTO
	name      string
	carriers  []mapPoolNode
	hosts     map[string]int64 // hostname → bytes
	dest      netip.Addr       // address of the busiest connection
	destHost  string
	destBytes int64
	destLoc   geo.Location
	destOK    bool
}

// buildMapFlows aggregates open connections into flows. Only proxied
// connections are tracked by liveConns, so every flow has an upstream.
func buildMapFlows(conns []liveFlow, env mapEnv, loc geo.Locator, p ipc.MapPathsParams) []ipc.MapFlowDTO {
	buckets := map[string]*flowBucket{}
	carriersOf := map[string][]mapPoolNode{} // per upstream, once per call
	for _, c := range conns {
		r, _ := env.rule(c.flow.RuleID)
		if p.Group != "" && r.group.key != p.Group {
			continue
		}
		if p.Iface != "" && r.iface != p.Iface {
			continue
		}
		carriers, seen := carriersOf[c.name]
		if !seen {
			carriers = env.poolCarriers(c.name)
			carriersOf[c.name] = carriers
		}
		poolKey := ""
		if len(carriers) > 0 {
			poolKey = carriers[0].key
		}
		dest, destOK := destAddr(c.name, c.flow, env)
		var dl geo.Location
		var located bool
		if destOK {
			dl, located = loc.Locate(dest)
		}
		key := strings.Join([]string{r.group.key, r.iface, c.name, poolKey, dl.Country, dl.City}, "\x00")
		b := buckets[key]
		if b == nil {
			b = &flowBucket{
				dto: ipc.MapFlowDTO{
					Group: r.group.key, GroupName: r.group.name, GroupColor: r.group.color,
					Iface: r.iface, Upstream: env.upstream(c.name).display,
				},
				name: c.name, carriers: carriers, hosts: map[string]int64{},
			}
			buckets[key] = b
		}
		b.dto.Conns++
		b.dto.RateUp += c.rateUp
		b.dto.RateDown += c.rateDown
		b.dto.BytesUp += c.up
		b.dto.BytesDown += c.down
		host := c.flow.Host
		if host == "" && c.flow.DestIP.IsValid() {
			host = c.flow.DestIP.String()
		}
		if host != "" {
			b.hosts[host] += c.up + c.down
		}
		// The dest hop shows the busiest connection, preferring one whose
		// address is known.
		bytes := c.up + c.down
		if b.destHost == "" || (destOK && !b.destOK) || (destOK == b.destOK && bytes > b.destBytes) {
			b.dest, b.destOK, b.destHost, b.destBytes = dest, destOK, host, bytes
			b.destLoc = geo.Location{}
			if located {
				b.destLoc = dl
			}
		}
	}

	pre := map[string][]ipc.MapHopDTO{} // you … exit, per upstream
	out := make([]ipc.MapFlowDTO, 0, len(buckets))
	for _, b := range buckets {
		hops, ok := pre[b.name]
		if !ok {
			hops = upstreamHops(b.name, b.carriers, env, loc)
			pre[b.name] = hops
		}
		dest := ipc.MapHopDTO{Role: ipc.MapHopDest, Label: b.destHost, Host: b.destHost}
		if b.destOK {
			dest.IP = b.dest.String()
			if b.destLoc.Precision != "" {
				fillLocation(&dest, b.destLoc)
			}
		}
		b.dto.Hops = append(append(make([]ipc.MapHopDTO, 0, len(hops)+1), hops...), dest)
		b.dto.Hosts = topHosts(b.hosts, mapHostsCap)
		out = append(out, b.dto)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i].RateUp+out[i].RateDown, out[j].RateUp+out[j].RateDown
		if ri != rj {
			return ri > rj
		}
		bi, bj := out[i].BytesUp+out[i].BytesDown, out[j].BytesUp+out[j].BytesDown
		if bi != bj {
			return bi > bj
		}
		return flowSortKey(out[i]) < flowSortKey(out[j])
	})
	if len(out) > mapFlowsCap {
		out = out[:mapFlowsCap]
	}
	return out
}

func flowSortKey(f ipc.MapFlowDTO) string {
	k := f.Group + "\x00" + f.Iface + "\x00" + f.Upstream
	if n := len(f.Hops); n > 0 {
		k += "\x00" + f.Hops[n-1].Country + "\x00" + f.Hops[n-1].City
	}
	return k
}

// destAddr is where a connection really goes: its destination IP unless
// that is a FakeIP handle, else its hostname resolved for display through
// the upstream carrying it (name).
func destAddr(name string, f connFlow, env mapEnv) (netip.Addr, bool) {
	if f.DestIP.IsValid() && !fakeIPPrefix.Contains(f.DestIP) {
		return f.DestIP, true
	}
	if f.Host == "" {
		return netip.Addr{}, false
	}
	return env.resolveDest(f.Host, name)
}

// upstreamHops builds the hops a flow through name takes before its
// destination: you, the pool node (masters), the server, the exit.
func upstreamHops(name string, carriers []mapPoolNode, env mapEnv, loc geo.Locator) []ipc.MapHopDTO {
	hops := make([]ipc.MapHopDTO, 0, 4)

	you := ipc.MapHopDTO{Role: ipc.MapHopYou, Label: "You"}
	if ip := env.publicIP(); ip != "" {
		if a, err := netip.ParseAddr(ip); err == nil {
			placeHop(&you, a, loc)
		}
	}
	hops = append(hops, you)

	if len(carriers) > 0 {
		n := carriers[0]
		// Certain only when the access log shows this node taking nearly
		// every recent pick; bytes alone can't say who xray picks next.
		sure := n.picks >= accessDominanceMin && n.share >= accessDominance
		pool := ipc.MapHopDTO{Role: ipc.MapHopPool, Label: n.label, Host: n.host, Uncertain: !sure}
		if a, ok := env.resolve(n.host); ok && n.host != "" {
			placeHop(&pool, a, loc)
		}
		for _, alt := range carriers[1:] {
			if sure || len(pool.Alternatives) == mapAlternativesCap {
				break
			}
			label := alt.label
			if alt.share > 0 {
				label += " (" + strconv.Itoa(int(math.Round(alt.share*100))) + "%)"
			}
			pool.Alternatives = append(pool.Alternatives, label)
		}
		hops = append(hops, pool)
	}

	u := env.upstream(name)
	server := ipc.MapHopDTO{Role: ipc.MapHopServer, Label: u.display, Host: u.serverHost}
	var serverIP netip.Addr
	if u.serverHost != "" {
		if a, ok := env.resolve(u.serverHost); ok {
			serverIP = a
			placeHop(&server, a, loc)
		}
	}
	hops = append(hops, server)

	if e := env.exitIP(name); e != "" {
		if a, err := netip.ParseAddr(e); err == nil && a.Unmap() != serverIP.Unmap() {
			exit := ipc.MapHopDTO{Role: ipc.MapHopExit, Label: "Exit " + u.display}
			placeHop(&exit, a, loc)
			hops = append(hops, exit)
		}
	}
	return hops
}

// placeHop sets a hop's IP and, when the Locator knows it, where it is.
func placeHop(h *ipc.MapHopDTO, a netip.Addr, loc geo.Locator) {
	a = a.Unmap()
	h.IP = a.String()
	if l, ok := loc.Locate(a); ok {
		fillLocation(h, l)
	}
}

func fillLocation(h *ipc.MapHopDTO, l geo.Location) {
	h.Country, h.CountryName, h.City = l.Country, l.CountryName, l.City
	h.Lat, h.Lon, h.Precision = l.Lat, l.Lon, l.Precision
	h.ASN, h.Org, h.Anycast = l.ASN, l.Org, l.Anycast
}

// topHosts returns up to n hostnames, most bytes first.
func topHosts(m map[string]int64, n int) []string {
	out := make([]string, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// noLocator answers nothing: the map still lists flows, unplaced.
type noLocator struct{}

func (noLocator) Locate(netip.Addr) (geo.Location, bool) { return geo.Location{}, false }

// hostResolver caches hostname → address for display, resolving misses in
// the background with a bounded number of lookups in flight. It is never
// consulted by routing. The cache is keyed by hostname alone: via only says
// which way the lookup goes, and whichever flow asked first fills it.
type hostResolver struct {
	lookup func(ctx context.Context, host, via string) (netip.Addr, error)
	bg     func() context.Context
	now    func() time.Time
	sem    chan struct{}

	mu sync.Mutex
	m  map[string]*hostEntry
}

type hostEntry struct {
	ip      netip.Addr
	until   time.Time
	pending bool
}

const (
	hostResolveOKTTL   = 10 * time.Minute
	hostResolveFailTTL = time.Minute
	hostResolveTimeout = 3 * time.Second
	hostResolveCap     = 4096 // cached names; past it new names wait for expiry
	hostResolveFlight  = 4    // lookups in flight
)

func newHostResolver(lookup func(ctx context.Context, host, via string) (netip.Addr, error), bg func() context.Context) *hostResolver {
	return &hostResolver{
		lookup: lookup, bg: bg, now: time.Now,
		sem: make(chan struct{}, hostResolveFlight),
		m:   map[string]*hostEntry{},
	}
}

// get returns host's cached address. A miss or an expired entry kicks one
// background lookup (if a slot is free) and returns what is known now —
// an expired address is still shown while its refresh runs.
func (r *hostResolver) get(host, via string) (netip.Addr, bool) {
	if host == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[host]
	if e != nil && (e.pending || now.Before(e.until)) {
		return e.ip, e.ip.IsValid()
	}
	if e == nil {
		if len(r.m) >= hostResolveCap {
			r.pruneLocked(now)
			if len(r.m) >= hostResolveCap {
				return netip.Addr{}, false
			}
		}
		e = &hostEntry{}
		r.m[host] = e
	}
	select {
	case r.sem <- struct{}{}:
	default:
		return e.ip, e.ip.IsValid() // busy: a later poll retries
	}
	e.pending = true
	go r.resolve(host, via, e)
	return e.ip, e.ip.IsValid()
}

func (r *hostResolver) resolve(host, via string, e *hostEntry) {
	defer func() { <-r.sem }()
	ctx, cancel := context.WithTimeout(r.bg(), hostResolveTimeout)
	defer cancel()
	a, err := r.lookup(ctx, host, via)
	r.mu.Lock()
	defer r.mu.Unlock()
	e.pending = false
	if err == nil && a.IsValid() {
		e.ip, e.until = a.Unmap(), r.now().Add(hostResolveOKTTL)
		return
	}
	e.until = r.now().Add(hostResolveFailTTL) // keep a previous address
}

func (r *hostResolver) pruneLocked(now time.Time) {
	for h, e := range r.m {
		if !e.pending && !now.Before(e.until) {
			delete(r.m, h)
		}
	}
}
