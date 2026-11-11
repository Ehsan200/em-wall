package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// Subscription exit measurement (docs/xray-sub-exit.md).
//
// A subscription used as a rule exit is only as good as its exit IPs: one
// shared IP is fine, several means a site sees the user hop between
// addresses (and often countries) from one connection to the next. Every
// active node of every enabled subscription is loaded as a probe-only
// outbound (xray.ExitProbeTag, reached through the probe inbound by SOCKS
// username), and this prober asks Cloudflare's trace endpoint through each
// one which IP it arrived from.
//
// The trace URL is an IPv4 literal on purpose: the node's server dials it
// itself, and a hostname could resolve to IPv6 there and report a v6
// address for an exit whose v4 address is what matters. It is also free of
// the per-minute limit ip-api has, which a 60-node pool would exhaust.
//
// Observations persist (xray_sub_exits) for subExitMemory, so a node whose
// IP rotates is caught across rounds and a daemon restart doesn't forget.

const (
	subExitProbeInterval = 15 * time.Minute
	subExitFirstDelay    = 60 * time.Second // let xray load the probe outbounds first
	subExitAfterRefresh  = 20 * time.Second
	subExitTimeout       = 6 * time.Second  // one trace request
	subExitNodeBudget    = 14 * time.Second // every URL for one node; a dead node costs this, not 3× the timeout
	subExitParallel      = 4
	subExitMemory        = 24 * time.Hour
)

// IPv4 literals first (see above); the hostname last for exits that can't
// reach those addresses — parseTrace still rejects an IPv6 answer.
var subExitTraceURLs = []string{
	"https://1.1.1.1/cdn-cgi/trace",
	"https://1.0.0.1/cdn-cgi/trace",
	"https://www.cloudflare.com/cdn-cgi/trace",
}

// subExitView is one subscription's verdict, computed from stored
// observations (subExitVerdict).
type subExitView struct {
	verdict    string // "single", "multi", "unverified"
	exits      []ipc.XraySubExitIPDTO
	rotating   []string // fingerprints
	measured   int
	unmeasured int
	needsAck   bool
	pinnedOnly bool
}

func (v subExitView) ips() []string {
	out := make([]string, len(v.exits))
	for i, e := range v.exits {
		out[i] = e.IP
	}
	return out
}

// subExitVerdict judges a subscription from its counted nodes (fps) and the
// remembered observations. ack is the accepted exit set.
func subExitVerdict(fps []string, obs []xray.SubExit, ack map[string]bool) subExitView {
	counted := make(map[string]bool, len(fps))
	for _, fp := range fps {
		counted[fp] = true
	}
	perNode := map[string]map[string]bool{}
	country := map[string]string{}
	for _, o := range obs {
		if !counted[o.Fingerprint] {
			continue
		}
		if perNode[o.Fingerprint] == nil {
			perNode[o.Fingerprint] = map[string]bool{}
		}
		perNode[o.Fingerprint][o.IP] = true
		if o.Country != "" {
			country[o.IP] = o.Country
		}
	}
	var v subExitView
	nodesPerIP := map[string]int{}
	for _, fp := range fps {
		ips := perNode[fp]
		if len(ips) == 0 {
			v.unmeasured++
			continue
		}
		v.measured++
		if len(ips) > 1 {
			v.rotating = append(v.rotating, fp)
		}
		for ip := range ips {
			nodesPerIP[ip]++
		}
	}
	for ip, n := range nodesPerIP {
		v.exits = append(v.exits, ipc.XraySubExitIPDTO{IP: ip, Country: country[ip], Nodes: n})
	}
	sort.Slice(v.exits, func(i, j int) bool {
		if v.exits[i].Nodes != v.exits[j].Nodes {
			return v.exits[i].Nodes > v.exits[j].Nodes
		}
		return v.exits[i].IP < v.exits[j].IP
	})
	switch {
	case v.measured == 0:
		v.verdict = "unverified"
	case len(v.exits) == 1:
		v.verdict = "single"
	default:
		v.verdict = "multi"
		for _, e := range v.exits {
			if !ack[e.IP] {
				v.needsAck = true
				break
			}
		}
	}
	return v
}

// subExitProber measures subscription exits and keeps the drift state.
type subExitProber struct {
	store   *xray.Store
	rules   *rules.Store
	running func() bool
	port    int // probe inbound; xray.ProbePort outside tests
	logger  *log.Logger

	kick chan int64 // subscription ID to measure now, 0 = all

	mu        sync.Mutex
	pending   map[int64]bool // asked for by MeasureAsync, not finished yet
	checking  map[int64]bool
	lastCheck map[int64]time.Time
	warned    map[int64]bool // NeedsAck last reported, for edge logging
}

func newSubExitProber(xs *xray.Store, rs *rules.Store, running func() bool, logger *log.Logger) *subExitProber {
	return &subExitProber{
		store: xs, rules: rs, running: running, port: xray.ProbePort, logger: logger,
		kick:      make(chan int64, 8),
		pending:   map[int64]bool{},
		checking:  map[int64]bool{},
		lastCheck: map[int64]time.Time{},
		warned:    map[int64]bool{},
	}
}

// Kick asks for a measurement of subscription id (0 = all) without waiting.
func (p *subExitProber) Kick(id int64) {
	if p == nil {
		return
	}
	select {
	case p.kick <- id:
	default:
	}
}

// KickAfter kicks once delay has passed (e.g. after a refresh, so xray has
// the new nodes' probe outbounds).
func (p *subExitProber) KickAfter(ctx context.Context, id int64, delay time.Duration) {
	if p == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(delay):
			p.Kick(id)
		}
	}()
}

func (p *subExitProber) run(ctx context.Context) {
	first := time.NewTimer(subExitFirstDelay)
	defer first.Stop()
	t := time.NewTicker(subExitProbeInterval)
	defer t.Stop()
	for {
		var id int64
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		case id = <-p.kick:
		}
		p.Measure(ctx, id)
	}
}

// MeasureAsync starts a measurement of subscription id (0 = all) in the
// background; the subscriptions it covers report Checking until it ends.
func (p *subExitProber) MeasureAsync(ctx context.Context, id int64) {
	if p == nil {
		return
	}
	subs, err := p.store.ListSubs(ctx)
	if err != nil {
		return
	}
	p.mu.Lock()
	for _, sub := range subs {
		if sub.Enabled && (id == 0 || sub.ID == id) {
			p.pending[sub.ID] = true
		}
	}
	p.mu.Unlock()
	go p.Measure(ctx, id)
}

// Measure probes every active node of subscription id (0 = every enabled
// subscription) and records what it saw. Synchronous.
func (p *subExitProber) Measure(ctx context.Context, id int64) {
	if p == nil {
		return
	}
	defer func() {
		p.mu.Lock()
		if id == 0 {
			p.pending = map[int64]bool{}
		} else {
			delete(p.pending, id)
		}
		p.mu.Unlock()
	}()
	if p.running != nil && !p.running() {
		return
	}
	subs, err := p.store.ListSubs(ctx)
	if err != nil {
		return
	}
	for _, sub := range subs {
		if !sub.Enabled || (id != 0 && sub.ID != id) {
			continue
		}
		p.measureSub(ctx, sub)
		if ctx.Err() != nil {
			return
		}
	}
	_ = p.store.PruneSubExits(ctx, time.Now().Add(-subExitMemory))
	p.noteDrift(ctx)
}

func (p *subExitProber) measureSub(ctx context.Context, sub xray.Subscription) {
	p.mu.Lock()
	if p.checking[sub.ID] {
		p.mu.Unlock()
		return
	}
	p.checking[sub.ID] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.checking, sub.ID)
		p.lastCheck[sub.ID] = time.Now()
		p.mu.Unlock()
	}()

	nodes, err := p.store.ActiveNodesForSubs(ctx, []string{sub.Name})
	if err != nil || len(nodes) == 0 {
		return
	}
	sem := make(chan struct{}, subExitParallel)
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(fp string) {
			defer wg.Done()
			defer func() { <-sem }()
			ip, loc, ok := traceExitVia(ctx, p.port, xray.ExitProbeTag(sub.ID, fp))
			if !ok {
				return
			}
			if err := p.store.RecordSubExit(ctx, sub.ID, fp, ip, loc, time.Now().UTC()); err != nil && p.logger != nil {
				p.logger.Printf("xray sub %s: record exit: %v", sub.Name, err)
			}
		}(n.Fingerprint)
	}
	wg.Wait()
}

// traceExitVia asks Cloudflare's trace endpoint, through the probe inbound
// as user (an outbound tag), which IPv4 address the request came from.
func traceExitVia(ctx context.Context, port int, user string) (ip, country string, ok bool) {
	d, err := proxy.NewDialer(proxy.Proxy{
		Protocol: proxy.ProtocolSOCKS5, Host: "127.0.0.1", Port: port,
		Username: user, Password: xray.ProbePassword,
	})
	if err != nil {
		return "", "", false
	}
	client := &http.Client{
		Timeout:   subExitTimeout,
		Transport: &http.Transport{DialContext: proxyDialContext(d), DisableKeepAlives: true},
	}
	ctx, cancel := context.WithTimeout(ctx, subExitNodeBudget)
	defer cancel()
	for _, u := range subExitTraceURLs {
		if ip, country, ok = fetchTrace(ctx, client, u); ok {
			return ip, country, true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return "", "", false
}

func fetchTrace(ctx context.Context, client *http.Client, url string) (ip, country string, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	return parseTrace(io.LimitReader(resp.Body, 8<<10))
}

// parseTrace reads "key=value" lines; ok only for an IPv4 ip=.
func parseTrace(r io.Reader) (ip, country string, ok bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, found := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !found {
			continue
		}
		switch k {
		case "ip":
			ip = v
		case "loc":
			country = strings.ToUpper(v)
		}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", "", false
	}
	return parsed.To4().String(), country, true
}

// views computes every subscription's verdict from the store.
func (p *subExitProber) views(ctx context.Context) (map[int64]subExitView, []xray.Subscription, error) {
	subs, err := p.store.ListSubs(ctx)
	if err != nil {
		return nil, nil, err
	}
	obs, err := p.store.SubExits(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[int64]subExitView, len(subs))
	for _, sub := range subs {
		fps, pinnedOnly := p.countedNodes(ctx, sub)
		v := subExitVerdict(fps, obs[sub.ID], xray.SplitExitIPs(sub.ExitAck))
		v.pinnedOnly = pinnedOnly
		out[sub.ID] = v
	}
	return out, subs, nil
}

// countedNodes are the nodes a verdict covers: the active ones (parked
// included — they come back on trial), or under the manual strategy with
// pins, the pinned ones only.
func (p *subExitProber) countedNodes(ctx context.Context, sub xray.Subscription) (fps []string, pinnedOnly bool) {
	nodes, err := p.store.ActiveNodesForSubs(ctx, []string{sub.Name})
	if err != nil {
		return nil, false
	}
	var pins map[string]bool
	if sub.EffectiveStrategy() == xray.StrategyManual {
		if pins, _ = p.store.PinnedFingerprints(ctx, sub.ID); len(pins) > 0 {
			pinnedOnly = true
		}
	}
	for _, n := range nodes {
		if pinnedOnly && !pins[n.Fingerprint] {
			continue
		}
		fps = append(fps, n.Fingerprint)
	}
	return fps, pinnedOnly
}

// view returns one subscription's verdict.
func (p *subExitProber) view(ctx context.Context, sub xray.Subscription) (subExitView, error) {
	obs, err := p.store.SubExits(ctx)
	if err != nil {
		return subExitView{}, err
	}
	fps, pinnedOnly := p.countedNodes(ctx, sub)
	v := subExitVerdict(fps, obs[sub.ID], xray.SplitExitIPs(sub.ExitAck))
	v.pinnedOnly = pinnedOnly
	return v, nil
}

// usedSubs counts enabled route rules per subscription name.
func (p *subExitProber) usedSubs(ctx context.Context) map[string]int {
	out := map[string]int{}
	if p.rules == nil {
		return out
	}
	list, err := p.rules.List(ctx)
	if err != nil {
		return out
	}
	for _, r := range list {
		if n := xray.ParseSubName(r.Interface); n != "" {
			out[n]++
		}
	}
	return out
}

// Warnings names the subscriptions an enabled rule routes through whose
// exits need a fresh confirmation.
func (p *subExitProber) Warnings(ctx context.Context) []string {
	if p == nil {
		return nil
	}
	views, subs, err := p.views(ctx)
	if err != nil {
		return nil
	}
	used := ruleExitNamesFromStore(ctx, p.rules)
	var out []string
	for _, sub := range subs {
		if views[sub.ID].needsAck && used[strings.ToLower(sub.Name)] {
			out = append(out, sub.Name)
		}
	}
	return out
}

func ruleExitNamesFromStore(ctx context.Context, rs *rules.Store) map[string]bool {
	if rs == nil {
		return map[string]bool{}
	}
	return storeRuleExits(rs)(ctx)
}

// noteDrift logs a subscription in use turning NeedsAck — new exit IPs the
// user hasn't accepted. Once per edge; the app shows the banner.
func (p *subExitProber) noteDrift(ctx context.Context) {
	views, subs, err := p.views(ctx)
	if err != nil {
		return
	}
	used := ruleExitNamesFromStore(ctx, p.rules)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sub := range subs {
		v := views[sub.ID]
		need := v.needsAck && used[strings.ToLower(sub.Name)]
		if need && !p.warned[sub.ID] && p.logger != nil {
			ack := xray.SplitExitIPs(sub.ExitAck)
			var fresh []string
			for _, e := range v.exits {
				if !ack[e.IP] {
					fresh = append(fresh, e.IP+exitCountrySuffix(e.Country))
				}
			}
			p.logger.Printf("xray sub %s: exits changed — now %d exit IPs, new: %s — rules keep routing; confirm in the app",
				sub.Name, len(v.exits), strings.Join(fresh, ", "))
		}
		p.warned[sub.ID] = need
	}
}

func exitCountrySuffix(cc string) string {
	if cc == "" {
		return ""
	}
	return " (" + cc + ")"
}

// DTOs builds the IPC view of every subscription.
func (p *subExitProber) DTOs(ctx context.Context) ([]ipc.XraySubExitDTO, error) {
	views, subs, err := p.views(ctx)
	if err != nil {
		return nil, err
	}
	used := p.usedSubs(ctx)
	names := poolNodeNamesFrom(ctx, p.store)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ipc.XraySubExitDTO, 0, len(subs))
	for _, sub := range subs {
		v := views[sub.ID]
		dto := ipc.XraySubExitDTO{
			SubID: sub.ID, Name: sub.Name, Enabled: sub.Enabled,
			Verdict:       v.verdict,
			Exits:         v.exits,
			RotatingNodes: []string{},
			Measured:      v.measured,
			Unmeasured:    v.unmeasured,
			Accepted:      []string{},
			NeedsAck:      v.needsAck,
			RuleCount:     used[strings.ToLower(sub.Name)],
			Checking:      p.checking[sub.ID] || p.pending[sub.ID],
			Strategy:      sub.EffectiveStrategy(),
			PinnedOnly:    v.pinnedOnly,
		}
		if dto.Exits == nil {
			dto.Exits = []ipc.XraySubExitIPDTO{}
		}
		for _, fp := range v.rotating {
			n := names[fp]
			if n == "" {
				n = fp
			}
			dto.RotatingNodes = append(dto.RotatingNodes, n)
		}
		for ip := range xray.SplitExitIPs(sub.ExitAck) {
			dto.Accepted = append(dto.Accepted, ip)
		}
		sort.Strings(dto.Accepted)
		if t, ok := p.lastCheck[sub.ID]; ok {
			dto.LastCheck = t.UTC().Format(time.RFC3339)
		}
		out = append(out, dto)
	}
	return out, nil
}

// multiExitError is what a rule write gets when it binds a multi-exit
// subscription without confirming. The "multi-exit:" prefix is the
// contract the app and CLI match on.
func multiExitError(sub string, v subExitView) error {
	var parts []string
	countries := map[string]bool{}
	for _, e := range v.exits {
		parts = append(parts, e.IP+exitCountrySuffix(e.Country))
		if e.Country != "" {
			countries[e.Country] = true
		}
	}
	return fmt.Errorf("multi-exit: subscription %q leaves the internet from %d different IPs across %d countries (%s); "+
		"sites may see your IP change between connections — confirm to use it",
		sub, len(v.exits), len(countries), strings.Join(parts, ", "))
}

// checkExitAck enforces the confirmation for a rule routed through a
// subscription exit: a multi-exit subscription whose exits aren't all
// accepted needs ack, and an ack accepts the current set. Unverified
// subscriptions pass (nothing to confirm yet).
func (d *handlerDeps) checkExitAck(ctx context.Context, iface string, ack bool) error {
	name := xray.ParseSubName(iface)
	if name == "" || d.subExits == nil {
		return nil
	}
	sub, err := d.xrayStore.GetSubByName(ctx, name)
	if err != nil {
		return nil // validateSubRefs already reported a missing one
	}
	v, err := d.subExits.view(ctx, sub)
	if err != nil || !v.needsAck {
		return err
	}
	if !ack {
		return multiExitError(sub.Name, v)
	}
	if err := d.xrayStore.AckSubExits(ctx, sub.ID, v.ips()); err != nil {
		return err
	}
	d.subExits.noteDrift(ctx)
	return nil
}
