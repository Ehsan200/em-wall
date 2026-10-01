package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// Parking dead pool nodes.
//
// A subscription pool routinely carries nodes that are simply gone (on the
// live install, 8 of 17 failed every ping for as long as the log went
// back). The burst observatory keeps pinging each of them every interval
// forever: a full outbound dial through the user's uplink, and a warning
// line in xray's error log, per dead node per interval. leastLoad already
// routes around them, so they cost probes and log volume and buy nothing.
//
// A node that has failed every ping for nodeDeadBeforePark is parked:
// left out of its slot's members, which the live apply turns into a plain
// outbound removal — no restart, nothing else touched. After its park time
// it is put back on trial; one live ping clears it, and failing through the
// trial parks it again for twice as long (capped). Parking never empties a
// pool and only happens while another member of the same slot is alive: if
// every node looks dead, the likelier cause is the user's own uplink, and
// parking would turn a local outage into a pool that stays empty after the
// link returns.
//
// Park times are short on purpose. Nodes on a flaky provider die and come
// back within the hour, and a parked node is invisible — it cannot win
// traffic however well it would do — so a long park costs more than the
// few pings a trial does. A real network change (or a wake from sleep)
// returns every parked node at once: "dead" was judged on the old network.

const (
	nodeHealthPollInterval = 30 * time.Second
	nodeDeadBeforePark     = 15 * time.Minute
	nodeTrialWindow        = 3 * time.Minute
	nodeParkInitial        = 5 * time.Minute
	nodeParkMax            = time.Hour
)

// nodeStatus is one outbound's entry in xray's /debug/vars "observatory".
type nodeStatus struct {
	Alive      bool  `json:"alive"`
	Delay      int64 `json:"delay"` // ms, last successful ping; set only while alive
	HealthPing struct {
		All  int `json:"all"`
		Fail int `json:"fail"`
		// Over the window's successful pings only, in ns. xray reports a
		// deviation of half the average when fewer than two succeeded.
		Average   int64 `json:"average"`
		Deviation int64 `json:"deviation"`
	} `json:"health_ping"`
}

// dead reports a node whose every recent ping failed. A node with no pings
// yet is unknown, not dead.
func (n nodeStatus) dead() bool {
	return !n.Alive && n.HealthPing.All > 0 && n.HealthPing.Fail == n.HealthPing.All
}

// parseObservatoryVars extracts tag → status from a /debug/vars body.
func parseObservatoryVars(b []byte) (map[string]nodeStatus, error) {
	m, err := parseMetricsVars(b)
	return m.observatory, err
}

// metricsVars is what the daemon reads from one /debug/vars body.
type metricsVars struct {
	observatory map[string]nodeStatus
	outbound    map[string]byteCount // per-outbound cumulative bytes, by tag
}

// parseMetricsVars extracts the observatory and the per-outbound byte
// counters (present when policy.system.statsOutbound* is on) from a
// /debug/vars body.
func parseMetricsVars(b []byte) (metricsVars, error) {
	var vars struct {
		Observatory map[string]nodeStatus `json:"observatory"`
		Stats       struct {
			Outbound map[string]struct {
				Uplink   int64 `json:"uplink"`
				Downlink int64 `json:"downlink"`
			} `json:"outbound"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(b, &vars); err != nil {
		return metricsVars{}, err
	}
	out := metricsVars{observatory: vars.Observatory, outbound: make(map[string]byteCount, len(vars.Stats.Outbound))}
	for tag, c := range vars.Stats.Outbound {
		out.outbound[tag] = byteCount{up: c.Uplink, down: c.Downlink}
	}
	return out, nil
}

type parkState struct {
	deadSince   time.Time     // first consecutive dead observation; zero = not dead
	parkedUntil time.Time     // zero = not parked
	parkFor     time.Duration // next park length (doubles per failed trial)
	onTrial     bool          // just returned from parking
}

// nodeParker decides which pool members (by DialerMember.Key) are parked.
// All methods are safe on a nil receiver, which parks nothing.
type nodeParker struct {
	mu    sync.Mutex
	now   func() time.Time
	nodes map[string]*parkState
}

func newNodeParker() *nodeParker {
	return &nodeParker{now: time.Now, nodes: map[string]*parkState{}}
}

// parked returns the keys currently parked.
func (p *nodeParker) parked() map[string]bool {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := map[string]bool{}
	for k, st := range p.nodes {
		if now.Before(st.parkedUntil) {
			out[k] = true
		}
	}
	return out
}

// parkedNode is one currently parked member, for the health view.
type parkedNode struct {
	Key   string
	Until time.Time
}

// list returns the parked members, soonest release first.
func (p *nodeParker) list() []parkedNode {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []parkedNode
	for k, st := range p.nodes {
		if now.Before(st.parkedUntil) {
			out = append(out, parkedNode{Key: k, Until: st.parkedUntil})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// parkEvent describes one decision, for logging.
type parkEvent struct {
	key    string
	parked bool // false = returned on trial
	for_   time.Duration
}

// observe folds one round of health into the park state and returns what
// changed. slots are the members currently loaded (parked ones are absent,
// so they are never observed while parked).
func (p *nodeParker) observe(slots []xray.DialerSlot, byTag map[string]nodeStatus) []parkEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var events []parkEvent

	for _, sl := range slots {
		status := make(map[string]nodeStatus, len(sl.Members))
		aliveCount := 0
		for _, m := range sl.Members {
			st, ok := byTag[xray.SlotMemberTag(sl.Index, m.Key)]
			if !ok {
				continue
			}
			status[m.Key] = st
			if st.Alive {
				aliveCount++
			}
		}
		remaining := len(sl.Members)
		for _, m := range sl.Members {
			st, ok := status[m.Key]
			if !ok {
				continue
			}
			ps := p.nodes[m.Key]
			switch {
			case st.Alive:
				delete(p.nodes, m.Key) // healthy: forget any history
			case st.dead():
				if ps == nil {
					ps = &parkState{}
					p.nodes[m.Key] = ps
				}
				if ps.deadSince.IsZero() {
					ps.deadSince = now
				}
				wait := nodeDeadBeforePark
				if ps.onTrial {
					wait = nodeTrialWindow
				}
				// Park only with a live sibling (uplink demonstrably up) and
				// never the pool's last member.
				if now.Sub(ps.deadSince) < wait || aliveCount == 0 || remaining <= 1 {
					continue
				}
				switch {
				case ps.parkFor == 0:
					ps.parkFor = nodeParkInitial
				case ps.onTrial:
					ps.parkFor *= 2
					if ps.parkFor > nodeParkMax {
						ps.parkFor = nodeParkMax
					}
				}
				ps.parkedUntil = now.Add(ps.parkFor)
				ps.deadSince = time.Time{}
				ps.onTrial = false
				remaining--
				events = append(events, parkEvent{key: m.Key, parked: true, for_: ps.parkFor})
			}
		}
	}
	return events
}

// release returns parked nodes whose time is up to the pool, on trial.
func (p *nodeParker) release() []parkEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var events []parkEvent
	for k, st := range p.nodes {
		if !st.parkedUntil.IsZero() && !now.Before(st.parkedUntil) {
			st.parkedUntil = time.Time{}
			st.onTrial = true
			st.deadSince = time.Time{}
			events = append(events, parkEvent{key: k})
		}
	}
	return events
}

// releaseAll returns every parked node to its pool, on trial, whatever its
// remaining park time. For a network change.
func (p *nodeParker) releaseAll() []parkEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var events []parkEvent
	for k, st := range p.nodes {
		if !st.parkedUntil.IsZero() {
			st.parkedUntil = time.Time{}
			st.onTrial = true
			st.deadSince = time.Time{}
			events = append(events, parkEvent{key: k})
		}
	}
	return events
}

// withoutParked drops parked members from a resolved member list, unless
// that would leave it empty.
func withoutParked(members []xray.DialerMember, parked map[string]bool) []xray.DialerMember {
	if len(parked) == 0 {
		return members
	}
	out := make([]xray.DialerMember, 0, len(members))
	for _, m := range members {
		if !parked[m.Key] {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return members
	}
	return out
}

// PollNodeHealth reads per-node health from xray's metrics endpoint and
// records it in the pool timeline (xray_timeline.go). Every
// nodeHealthPollInterval it also parks nodes that have stayed dead, returns
// parked nodes whose time is up, re-ranks each slot's shortlist
// (xray_shortlist.go), and applies any change live. Called on
// nodeTimelineInterval from main.go; the slower decisions keep their own
// cadence so a finer timeline doesn't make the shortlist twitchier.
func (s *xraySupervisor) PollNodeHealth(ctx context.Context) {
	s.mu.Lock()
	running := s.enabled && s.cmd != nil
	slots := s.loadedSlots
	s.mu.Unlock()
	if !running {
		return
	}

	var byTag map[string]nodeStatus
	if len(slots) > 0 {
		m, err := s.fetchMetrics(ctx)
		if err != nil {
			return // metrics not up yet (first seconds after a start)
		}
		byTag = m.observatory
		s.timeline.record(slots, byTag, m.outbound, s.parker.parked())
	}
	// Half a tick of slack, so ticker jitter doesn't skip a whole round.
	now := time.Now()
	if !s.lastDecide.IsZero() && now.Sub(s.lastDecide) < nodeHealthPollInterval-nodeTimelineInterval/2 {
		return
	}
	s.lastDecide = now

	var events []parkEvent
	var moved []shortlistChange
	if len(slots) > 0 {
		events = s.parker.observe(slots, byTag)
		moved = s.shortlist.observe(slots, byTag)
	}
	events = append(events, s.parker.release()...)
	if len(events) == 0 && len(moved) == 0 {
		return
	}
	for _, c := range moved {
		s.logger.Printf("xray supervisor: master %s shortlist %v → %v", c.master, c.from, c.to)
	}
	for _, e := range events {
		if e.parked {
			s.logger.Printf("xray supervisor: pool node %s dead — parked for %s", e.key, e.for_)
		} else {
			s.logger.Printf("xray supervisor: pool node %s back on trial", e.key)
		}
	}
	if err := s.Reconcile(ctx); err != nil {
		s.logger.Printf("xray supervisor: apply node parking: %v", err)
	}
}

// ReturnParked puts every parked pool node back on trial and applies it
// live. Called on a real network change: a node judged dead on the old
// network may well work on this one, and waiting out its park time would
// keep it out of its pool for up to nodeParkMax.
func (s *xraySupervisor) ReturnParked(ctx context.Context) {
	events := s.parker.releaseAll()
	if len(events) == 0 {
		return
	}
	s.logger.Printf("xray supervisor: network changed — %d parked pool node(s) back on trial", len(events))
	if err := s.Reconcile(ctx); err != nil {
		s.logger.Printf("xray supervisor: return parked nodes: %v", err)
	}
}

// NodePings reports the live health-ping result of every pool member,
// keyed by member key, plus the parked ones. Empty when xray isn't
// running or its metrics aren't up yet.
func (s *xraySupervisor) NodePings(ctx context.Context) map[string]ipc.XrayNodePing {
	out := map[string]ipc.XrayNodePing{}
	s.mu.Lock()
	running := s.enabled && s.cmd != nil
	s.mu.Unlock()
	if running {
		byTag, _ := s.fetchObservatory(ctx)
		for tag, st := range byTag {
			key, ok := xray.SlotMemberKey(tag)
			if !ok {
				continue
			}
			p := ipc.XrayNodePing{LatencyMs: -1, Down: st.dead()}
			if st.Alive && st.Delay > 0 {
				p.LatencyMs = int(st.Delay)
			}
			// A member shared by two slots is pinged in each; keep the
			// better reading.
			if prev, seen := out[key]; seen && !betterPing(p, prev) {
				continue
			}
			out[key] = p
		}
	}
	for _, pn := range s.parker.list() {
		out[pn.Key] = ipc.XrayNodePing{LatencyMs: -1, Down: true, Parked: true}
	}
	return out
}

func betterPing(a, b ipc.XrayNodePing) bool {
	switch {
	case a.LatencyMs >= 0 && b.LatencyMs < 0:
		return true
	case a.LatencyMs < 0 || b.LatencyMs < 0:
		return !a.Down && b.Down
	}
	return a.LatencyMs < b.LatencyMs
}

func (s *xraySupervisor) fetchObservatory(ctx context.Context) (map[string]nodeStatus, error) {
	m, err := s.fetchMetrics(ctx)
	return m.observatory, err
}

func (s *xraySupervisor) fetchMetrics(ctx context.Context) (metricsVars, error) {
	addr := s.metricsAddr
	if addr == "" {
		addr = "127.0.0.1:" + strconv.Itoa(xray.MetricsPort)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/debug/vars", nil)
	if err != nil {
		return metricsVars{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return metricsVars{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsVars{}, fmt.Errorf("metrics: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return metricsVars{}, err
	}
	return parseMetricsVars(b)
}
