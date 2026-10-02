package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

// Pool path prober.
//
// The burst observatory judges a pool node by one generate_204 through the
// node alone: a few hundred bytes. Two kinds of node pass that and still
// can't carry a master, and the shortlist — which ranks on those pings —
// preferred exactly them (observed: every master of a pool on the two
// fastest-pinging nodes, neither able to finish a TLS handshake):
//
//   - a node whose path chokes after a few KB. A filtering home ISP let a
//     raw-TCP VLESS node move 7–13 KB per connection and then stalled it;
//     the same node worked on cellular. The ping finishes before the cut.
//   - a node that can't reach one master's server, while it reaches the
//     probe URL and other masters fine.
//
// So the daemon tests the real path itself, through the probe inbound
// (xray.ProbePort, routed by SOCKS username): a transfer test of
// netprobe.DownloadTestBytes through each member, and a URL test through
// each master chained over that one member (xray.ChainProbeTag). Members
// that fail are reported as DialerSlot.Broken, which Generate costs last,
// the shortlist and the agile pick skip, and the fallback avoids.
//
// Cost is bounded: each round probes a slot's carriers (shortlist, agile
// pick, fallback) and at most pathProbeCandidates of the next-best members
// whose verdict is missing, pending or stale, so a pool of twenty doesn't
// pull twenty transfers a minute through the user's subscription.
//
// A failure is only a strike when the same round shows the path works
// through other members (see record) — the uplink argument used everywhere
// else: if nothing passes, it's the local link (or, for a chain, the
// master's own server), and blaming every member would mark the whole pool
// broken at once. pathStrikes in a row make a member broken; one pass
// clears it. Verdicts are dropped on a real network change, since a node
// choked on one network may work on the next.

const (
	pathProbeInterval   = 60 * time.Second
	pathProbeTimeout    = 8 * time.Second
	pathProbeParallel   = 4
	pathProbeCandidates = 4
	pathStrikes         = 2
	// A non-carrier's pass is trusted this long before it is re-tested; a
	// broken member is re-tested after pathBadRecheck. Carriers are tested
	// every round.
	pathGoodTTL    = 10 * time.Minute
	pathBadRecheck = 5 * time.Minute
)

type pathRecord struct {
	strikes   int
	bad       bool
	lastProbe time.Time
}

// due reports whether a non-carrier member's record wants a test now.
func (r *pathRecord) due(now time.Time) bool {
	switch {
	case r == nil || r.lastProbe.IsZero():
		return true
	case r.bad:
		return now.Sub(r.lastProbe) >= pathBadRecheck
	case r.strikes > 0:
		return true // confirm or clear a strike on the next round
	default:
		return now.Sub(r.lastProbe) >= pathGoodTTL
	}
}

// pathProber keeps the verdicts. Nil-safe: a nil prober reports nothing
// broken and plans nothing.
type pathProber struct {
	mu    sync.Mutex
	now   func() time.Time
	node  map[string]*pathRecord            // member key → transfer verdict
	chain map[string]map[string]*pathRecord // master → member key → chain verdict
}

func newPathProber() *pathProber {
	return &pathProber{
		now:   time.Now,
		node:  map[string]*pathRecord{},
		chain: map[string]map[string]*pathRecord{},
	}
}

// pathJob is one test: a transfer through a member (master == ""), or a
// URL test through master chained over the member.
type pathJob struct {
	user   string // probe inbound username = outbound tag
	key    string
	master string
}

type pathResult struct {
	pathJob
	ok bool
}

// pathEdge is a member turning broken or clean, for logging.
type pathEdge struct {
	key    string
	master string // "" = the transfer test
	bad    bool
}

// broken returns slot's members that failed either test — the transfer
// test, or the chain test for any master on the slot — in member order.
func (p *pathProber) broken(slot xray.DialerSlot) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range slot.Members {
		bad := p.node[m.Key] != nil && p.node[m.Key].bad
		for _, master := range slot.SlotMasters() {
			if r := p.chain[strings.ToLower(master)][m.Key]; r != nil && r.bad {
				bad = true
			}
		}
		if bad {
			out = append(out, m.Key)
		}
	}
	return out
}

// plan picks this round's tests. Slots with a single member are skipped:
// there is nothing to choose between.
func (p *pathProber) plan(slots []xray.DialerSlot, byTag map[string]nodeStatus) []pathJob {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var jobs []pathJob
	nodeSeen := map[string]bool{}
	live := map[string]bool{}
	liveMasters := map[string]bool{}
	for _, slot := range slots {
		for _, m := range slot.Members {
			live[m.Key] = true
		}
		for _, master := range slot.SlotMasters() {
			liveMasters[strings.ToLower(master)] = true
		}
		if len(slot.Members) < 2 {
			continue
		}
		for _, key := range p.pickLocked(slot, byTag, now) {
			if !nodeSeen[key] {
				nodeSeen[key] = true
				jobs = append(jobs, pathJob{user: xray.SlotMemberTag(slot.Index, key), key: key})
			}
			for _, master := range slot.SlotMasters() {
				jobs = append(jobs, pathJob{user: xray.ChainProbeTag(slot.Index, master, key), key: key, master: strings.ToLower(master)})
			}
		}
	}
	// Forget members and masters no slot has any more.
	for k := range p.node {
		if !live[k] {
			delete(p.node, k)
		}
	}
	for m, recs := range p.chain {
		if !liveMasters[m] {
			delete(p.chain, m)
			continue
		}
		for k := range recs {
			if !live[k] {
				delete(recs, k)
			}
		}
	}
	return jobs
}

// pickLocked returns the members of slot to test this round: its carriers,
// then up to pathProbeCandidates more, best observatory score first, whose
// verdict is due. Members the observatory can't score (dead, too few
// pings) are left to parking.
func (p *pathProber) pickLocked(slot xray.DialerSlot, byTag map[string]nodeStatus, now time.Time) []string {
	carriers := map[string]bool{}
	for _, k := range slot.Preferred {
		carriers[k] = true
	}
	if slot.Fallback != "" {
		carriers[slot.Fallback] = true
	}
	if len(carriers) == 0 && len(slot.Members) <= xray.SlotShortlistSize {
		for _, m := range slot.Members {
			carriers[m.Key] = true // no shortlist: the balancer spreads over all of them
		}
	}
	due := func(key string) bool {
		if p.node[key].due(now) {
			return true
		}
		for _, master := range slot.SlotMasters() {
			if p.chain[strings.ToLower(master)][key].due(now) {
				return true
			}
		}
		return false
	}
	type cand struct {
		key   string
		score time.Duration
	}
	var out []string
	var cands []cand
	for _, m := range slot.Members {
		if carriers[m.Key] {
			out = append(out, m.Key)
			continue
		}
		st, ok := byTag[xray.SlotMemberTag(slot.Index, m.Key)]
		if !ok {
			continue
		}
		score, ok := nodeScore(st)
		if !ok || !due(m.Key) {
			continue
		}
		cands = append(cands, cand{m.Key, score})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return cands[i].key < cands[j].key
	})
	for i := 0; i < len(cands) && i < pathProbeCandidates; i++ {
		out = append(out, cands[i].key)
	}
	return out
}

// record folds one round's results in and returns the members whose
// verdict flipped.
//
// A failure is a strike only when the same round shows the path works
// through another member: a transfer that passed (else it's the link), or
// for a chain, the same master carried through another member that isn't
// choking its transfer (else it's the master's own server or config).
func (p *pathProber) record(results []pathResult) []pathEdge {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	transferPass := 0
	choked := map[string]bool{}
	for _, r := range results {
		if r.master == "" {
			if r.ok {
				transferPass++
			} else {
				choked[r.key] = true
			}
		}
	}
	chainPass := map[string]bool{}
	for _, r := range results {
		if r.master != "" && r.ok && !choked[r.key] {
			chainPass[r.master] = true
		}
	}

	var edges []pathEdge
	for _, r := range results {
		var rec *pathRecord
		var witnessed bool
		if r.master == "" {
			if p.node[r.key] == nil {
				p.node[r.key] = &pathRecord{}
			}
			rec = p.node[r.key]
			witnessed = transferPass > 0
		} else {
			if p.chain[r.master] == nil {
				p.chain[r.master] = map[string]*pathRecord{}
			}
			if p.chain[r.master][r.key] == nil {
				p.chain[r.master][r.key] = &pathRecord{}
			}
			rec = p.chain[r.master][r.key]
			witnessed = chainPass[r.master]
		}
		rec.lastProbe = now
		if r.ok {
			rec.strikes = 0
			if rec.bad {
				rec.bad = false
				edges = append(edges, pathEdge{key: r.key, master: r.master})
			}
			continue
		}
		if !witnessed {
			continue // the link, or the master's own server: not this member
		}
		rec.strikes++
		if rec.strikes >= pathStrikes && !rec.bad {
			rec.bad = true
			edges = append(edges, pathEdge{key: r.key, master: r.master, bad: true})
		}
	}
	return edges
}

// reset drops every verdict: called on a real network change. It reports
// whether any member was broken, i.e. whether the config needs a reapply.
func (p *pathProber) reset() (hadBroken bool) {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.node {
		hadBroken = hadBroken || r.bad
	}
	for _, recs := range p.chain {
		for _, r := range recs {
			hadBroken = hadBroken || r.bad
		}
	}
	p.node = map[string]*pathRecord{}
	p.chain = map[string]map[string]*pathRecord{}
	return hadBroken
}

// ProbePaths runs one path-probe round and applies any verdict change
// live. Called every pathProbeInterval from main.go.
func (s *xraySupervisor) ProbePaths(ctx context.Context) {
	s.mu.Lock()
	running := s.enabled && s.cmd != nil
	slots := s.loadedSlots
	s.mu.Unlock()
	if !running || len(slots) == 0 {
		return
	}
	byTag, err := s.fetchObservatory(ctx)
	if err != nil {
		return
	}
	jobs := s.paths.plan(slots, byTag)
	if len(jobs) == 0 {
		return
	}
	results := runPathJobs(ctx, jobs, xray.ProbePort)
	if ctx.Err() != nil {
		return
	}
	edges := s.paths.record(results)
	if len(edges) == 0 {
		return
	}
	names := poolNodeNamesFrom(ctx, s.xrayStore)
	name := func(k string) string {
		if n := names[k]; n != "" {
			return n
		}
		return memberDisplayName(k)
	}
	for _, e := range edges {
		what := "stalls a " + strconv.Itoa(netprobe.DownloadTestBytes>>10) + " KB transfer"
		if e.master != "" {
			what = "can't carry master " + e.master
		}
		if e.bad {
			s.logger.Printf("xray pool: node %s %s — ranked last until it passes again", name(e.key), what)
		} else {
			s.logger.Printf("xray pool: node %s passes again (%s)", name(e.key), strings.Replace(what, "can't carry", "carries", 1))
		}
	}
	if err := s.Reconcile(ctx); err != nil {
		s.logger.Printf("xray supervisor: apply path verdicts: %v", err)
	}
}

// runPathJobs runs jobs through the probe inbound on port, at most
// pathProbeParallel at a time.
func runPathJobs(ctx context.Context, jobs []pathJob, port int) []pathResult {
	out := make([]pathResult, len(jobs))
	sem := make(chan struct{}, pathProbeParallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		out[i].pathJob = j
		d, err := proxy.NewDialer(proxy.Proxy{
			Protocol: proxy.ProtocolSOCKS5, Host: "127.0.0.1", Port: port,
			Username: j.user, Password: xray.ProbePassword,
		})
		if err != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d proxy.Dialer) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, pathProbeTimeout)
			defer cancel()
			var r netprobe.Result
			if out[i].master == "" {
				r = netprobe.MeasureDownload(pctx, d, netprobe.DownloadTestHost, netprobe.DownloadTestPort,
					netprobe.DownloadTestPath(netprobe.DownloadTestBytes), netprobe.DownloadTestBytes)
			} else {
				r = netprobe.MeasureURL(pctx, d, netprobe.URLTestHost, netprobe.URLTestPort, netprobe.URLTestPath)
			}
			out[i].ok = r.OK
		}(i, d)
	}
	wg.Wait()
	return out
}
