package main

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// Pool health timeline.
//
// Parking and the shortlist assume a pool whose nodes are mostly steady:
// dead ones stay dead, live ones stay live. Some subscriptions don't behave
// like that — reportedly a batch of nodes works for a minute, dies, and a
// different batch takes over, in waves. Before any switching strategy is
// built for that, this records what a pool actually does, so the strategy
// can be judged (and tuned) against data rather than a description.
//
// Every nodeTimelineInterval the supervisor's health poll hands over one
// round of xray's observatory and per-outbound byte counters. For each
// loaded slot it keeps a ring of samples: each member's probe state, its
// role in the balancer (fallback / shortlisted / ranked out), its window
// RTT and the bytes it moved since the previous sample. The bytes are what
// probes can't show: a node whose pings pass while the traffic it carries
// gets nothing back (uplink growing, downlink flat).
//
// Each pool also carries its masters' standing in the set ranking (probe
// and traffic verdicts from netprobe) on the same time axis, the samples
// where the shortlist was swapped, and every master outage / demotion with
// what the pool looked like when it began. That is what tells a failing
// master server apart from a dying pool node or our own routing swap.
//
// Observation only. Nothing reads it to make a decision yet. In memory; a
// daemon restart starts a fresh history.

const (
	nodeTimelineInterval = 10 * time.Second
	nodeTimelineSpan     = 30 * time.Minute
	nodeTimelineCap      = int(nodeTimelineSpan / nodeTimelineInterval)

	// nearSwapWindow: a master outage starting this soon after a shortlist
	// swap is counted as possibly caused by it.
	nearSwapWindow = time.Minute
)

type nodeCell struct {
	state, role byte
	rttMs       int
	up, down    int64
}

type poolSample struct {
	at         time.Time
	uplinkDown bool
	swap       bool                // the shortlist was changed after this sample
	nodes      map[string]nodeCell // member key → cell; parked members included
	masters    map[string]byte     // master name → ipc.MasterCell*
}

// masterEvent is one outage start or demotion of a master riding a pool.
type masterEvent struct {
	at        time.Time
	master    string
	demotion  bool // false = outage start
	carrierUp bool // a carrying node was answering when it began
	nearSwap  bool // within nearSwapWindow of a shortlist swap
}

type poolHistory struct {
	masters  []string
	samples  []poolSample  // oldest first, at most nodeTimelineCap
	events   []masterEvent // oldest first, within nodeTimelineSpan
	lastSwap time.Time
}

// masterStanding is a master's verdict in the set ranking right now.
type masterStanding struct{ down, demoted bool }

// masterMark is the worst a master was in since the last sample: the
// tracker's edges land between samples, and a few-second outage would
// otherwise never show on the strip.
type masterMark struct{ down, demoted bool }

// byteCount is one outbound's cumulative counters.
type byteCount struct{ up, down int64 }

// poolTimeline holds the history of every loaded pool, keyed by the slot's
// owning master. Safe on a nil receiver, which records nothing.
type poolTimeline struct {
	mu    sync.Mutex
	now   func() time.Time
	pools map[string]*poolHistory
	last  map[string]byteCount  // outbound tag → counters at the previous sample
	marks map[string]masterMark // lower-case master name → worst since last sample
}

func newPoolTimeline() *poolTimeline {
	return &poolTimeline{now: time.Now, pools: map[string]*poolHistory{}, last: map[string]byteCount{}, marks: map[string]masterMark{}}
}

// record folds one round into the history. slots are the members loaded in
// xray right now (parked ones absent); traffic is xray's per-outbound
// cumulative counters by tag; standing is each master's current verdict
// by lower-case name (nil or missing = not measured).
func (t *poolTimeline) record(slots []xray.DialerSlot, byTag map[string]nodeStatus, traffic map[string]byteCount, parked map[string]bool, standing map[string]masterStanding) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	live := make(map[string]bool, len(slots))
	nextLast := make(map[string]byteCount, len(traffic))
	for _, slot := range slots {
		live[slot.Master] = true
		h := t.pools[slot.Master]
		if h == nil {
			h = &poolHistory{}
			t.pools[slot.Master] = h
		}
		h.masters = slot.SlotMasters()

		roles := slotRoles(slot)
		smp := poolSample{at: now, nodes: make(map[string]nodeCell, len(slot.Members)), masters: make(map[string]byte, len(h.masters))}
		for _, m := range h.masters {
			lm := strings.ToLower(m)
			st, known := standing[lm]
			mk := t.marks[lm]
			switch {
			case st.down || mk.down:
				smp.masters[m] = ipc.MasterCellDown
			case st.demoted || mk.demoted:
				smp.masters[m] = ipc.MasterCellDemoted
			case known:
				smp.masters[m] = ipc.MasterCellOK
			default:
				smp.masters[m] = ipc.MasterCellUnknown
			}
		}
		answered, failed := 0, 0
		for _, m := range slot.Members {
			tag := xray.SlotMemberTag(slot.Index, m.Key)
			c := nodeCell{state: ipc.PoolCellUnknown, role: roles[m.Key]}
			if st, ok := byTag[tag]; ok {
				c.state = cellState(st)
				if c.state == ipc.PoolCellAlive || c.state == ipc.PoolCellFlaky {
					c.rttMs = int(time.Duration(st.HealthPing.Average) / time.Millisecond)
					if c.rttMs <= 0 && st.Delay > 0 {
						c.rttMs = int(st.Delay)
					}
				}
			}
			switch c.state {
			case ipc.PoolCellAlive, ipc.PoolCellFlaky:
				answered++
			case ipc.PoolCellDead:
				failed++
			}
			if cur, ok := traffic[tag]; ok {
				nextLast[tag] = cur
				// No previous reading: nothing to diff against. A counter
				// that went backwards means xray restarted and began again.
				if prev, ok := t.last[tag]; ok {
					c.up, c.down = counterDelta(prev.up, cur.up), counterDelta(prev.down, cur.down)
				}
			}
			smp.nodes[m.Key] = c
		}
		// A parked member is gone from Members; carry it forward from the
		// previous sample so a node's whole story stays on one line.
		if n := len(h.samples); n > 0 {
			for k := range h.samples[n-1].nodes {
				if _, in := smp.nodes[k]; !in && parked[k] {
					smp.nodes[k] = nodeCell{state: ipc.PoolCellParked, role: ipc.PoolRoleNone}
				}
			}
		}
		smp.uplinkDown = answered == 0 && failed > 0
		h.samples = append(h.samples, smp)
		if over := len(h.samples) - nodeTimelineCap; over > 0 {
			h.samples = append(h.samples[:0], h.samples[over:]...)
		}
		cut := 0
		for cut < len(h.events) && now.Sub(h.events[cut].at) > nodeTimelineSpan {
			cut++
		}
		h.events = append(h.events[:0], h.events[cut:]...)
	}
	for m := range t.pools {
		if !live[m] {
			delete(t.pools, m)
		}
	}
	t.last = nextLast
	clear(t.marks)
}

// markSwap notes that the pool owned by master just had its shortlist
// changed.
func (t *poolTimeline) markSwap(master string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.pools[master]
	if h == nil {
		return
	}
	h.lastSwap = t.now()
	if n := len(h.samples); n > 0 {
		h.samples[n-1].swap = true
	}
}

// poolCarrier is one carrying node's latest state, for the outage log line.
type poolCarrier struct {
	key   string
	state byte
	rttMs int
}

// noteMaster records a master's outage start (demotion = false) or
// demotion, as reported by the latency tracker's edge hooks. It returns
// the pool's carrying nodes at that moment and whether the shortlist was
// swapped within nearSwapWindow; ok is false when master rides no pool.
func (t *poolTimeline) noteMaster(master string, demotion bool) (carriers []poolCarrier, sinceSwap time.Duration, ok bool) {
	if t == nil {
		return nil, 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	lm := strings.ToLower(master)
	mk := t.marks[lm]
	if demotion {
		mk.demoted = true
	} else {
		mk.down = true
	}
	t.marks[lm] = mk

	var h *poolHistory
	for _, p := range t.pools {
		for _, m := range p.masters {
			if strings.EqualFold(m, master) {
				h, master = p, m
			}
		}
	}
	if h == nil {
		return nil, 0, false
	}
	now := t.now()
	ev := masterEvent{at: now, master: master, demotion: demotion}
	sinceSwap = -1
	if !h.lastSwap.IsZero() {
		sinceSwap = now.Sub(h.lastSwap)
		ev.nearSwap = sinceSwap <= nearSwapWindow
	}
	if n := len(h.samples); n > 0 {
		for k, c := range h.samples[n-1].nodes {
			if c.role != ipc.PoolRoleFallback && c.role != ipc.PoolRoleActive {
				continue
			}
			carriers = append(carriers, poolCarrier{key: k, state: c.state, rttMs: c.rttMs})
			if c.state == ipc.PoolCellAlive || c.state == ipc.PoolCellFlaky {
				ev.carrierUp = true
			}
		}
	}
	sort.Slice(carriers, func(i, j int) bool { return carriers[i].key < carriers[j].key })
	h.events = append(h.events, ev)
	return carriers, sinceSwap, true
}

func counterDelta(prev, cur int64) int64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// cellState classifies one observatory entry.
func cellState(st nodeStatus) byte {
	hp := st.HealthPing
	switch {
	case hp.All == 0:
		return ipc.PoolCellUnknown
	case hp.Fail >= hp.All:
		return ipc.PoolCellDead
	case hp.Fail > 0:
		return ipc.PoolCellFlaky
	}
	return ipc.PoolCellAlive
}

// slotRoles mirrors how Generate wires the slot's balancer: the shortlist
// (Preferred) carries traffic with its head as fallbackTag; without one, a
// slot of at most SlotShortlistSize members is carried by all of them and a
// bigger one is left to leastLoad, with the first member as fallback.
func slotRoles(slot xray.DialerSlot) map[string]byte {
	roles := make(map[string]byte, len(slot.Members))
	have := make(map[string]bool, len(slot.Members))
	for _, m := range slot.Members {
		have[m.Key] = true
	}
	var pref []string
	for _, k := range slot.Preferred {
		if have[k] {
			pref = append(pref, k)
		}
	}
	switch {
	case len(pref) > 0:
		for _, m := range slot.Members {
			roles[m.Key] = ipc.PoolRoleIdle
		}
		for _, k := range pref {
			roles[k] = ipc.PoolRoleActive
		}
		roles[pref[0]] = ipc.PoolRoleFallback
	case len(slot.Members) <= xray.SlotShortlistSize:
		for _, m := range slot.Members {
			roles[m.Key] = ipc.PoolRoleActive
		}
	default:
		for _, m := range slot.Members {
			roles[m.Key] = ipc.PoolRoleUnranked
		}
	}
	if len(pref) == 0 && len(slot.Members) > 0 {
		roles[slot.Members[0].Key] = ipc.PoolRoleFallback
	}
	// An agile pool's fallback is its spare, not its fastest member.
	if slot.Fallback != "" && have[slot.Fallback] {
		for k, r := range roles {
			if r == ipc.PoolRoleFallback {
				roles[k] = ipc.PoolRoleActive
			}
		}
		roles[slot.Fallback] = ipc.PoolRoleFallback
	}
	return roles
}

// snapshot renders the pools' history over the last window (≤ 0 = all of
// it). master, when set, picks the pool it owns or shares. names maps member
// keys to display names; unnamed keys fall back to memberDisplayName.
func (t *poolTimeline) snapshot(master string, window time.Duration, names map[string]string) []ipc.PoolTimelineDTO {
	out := []ipc.PoolTimelineDTO{}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	since := time.Time{}
	if window > 0 {
		since = t.now().Add(-window)
	}
	owners := make([]string, 0, len(t.pools))
	for o := range t.pools {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		h := t.pools[owner]
		if master != "" && !slices.Contains(h.masters, master) {
			continue
		}
		var smps []poolSample
		for _, s := range h.samples {
			if !s.at.Before(since) {
				smps = append(smps, s)
			}
		}
		dto := renderPool(owner, h.masters, smps, names)
		dto.MasterRows = renderMasters(h.masters, smps, h.events, since)
		out = append(out, dto)
	}
	return out
}

func renderPool(owner string, masters []string, smps []poolSample, names map[string]string) ipc.PoolTimelineDTO {
	dto := ipc.PoolTimelineDTO{
		Master:      owner,
		Masters:     append([]string(nil), masters...),
		IntervalSec: int(nodeTimelineInterval / time.Second),
		Times:       make([]int64, len(smps)),
		UplinkDown:  make([]bool, len(smps)),
		Swaps:       make([]bool, len(smps)),
		Nodes:       []ipc.PoolNodeTimelineDTO{},
		MasterRows:  []ipc.PoolMasterTimelineDTO{},
	}
	keys := map[string]bool{}
	for i, s := range smps {
		dto.Times[i] = s.at.Unix()
		dto.UplinkDown[i] = s.uplinkDown
		dto.Swaps[i] = s.swap
		for k := range s.nodes {
			keys[k] = true
		}
	}

	lost := false // inside a pick-loss episode
	for _, s := range smps {
		if s.uplinkDown {
			continue
		}
		carriers, carriersDown, othersUp := 0, 0, false
		for _, c := range s.nodes {
			up := c.state == ipc.PoolCellAlive || c.state == ipc.PoolCellFlaky
			if c.role == ipc.PoolRoleFallback || c.role == ipc.PoolRoleActive {
				carriers++
				if c.state == ipc.PoolCellDead {
					carriersDown++
				}
			} else if up {
				othersUp = true
			}
		}
		now := carriers > 0 && carriersDown == carriers && othersUp
		if now && !lost {
			dto.PickLosses++
		}
		lost = now
	}

	flipped := 0
	for k := range keys {
		n := ipc.PoolNodeTimelineDTO{
			Key:       k,
			Name:      names[k],
			RTTMs:     make([]int, len(smps)),
			UpBytes:   make([]int64, len(smps)),
			DownBytes: make([]int64, len(smps)),
		}
		if n.Name == "" {
			n.Name = memberDisplayName(k)
		}
		states := make([]byte, len(smps))
		roles := make([]byte, len(smps))
		observed, answered, rttSum, rttN := 0, 0, 0, 0
		var prevUp *bool
		for i, s := range smps {
			c, ok := s.nodes[k]
			if !ok {
				c = nodeCell{state: ipc.PoolCellAbsent, role: ipc.PoolRoleNone}
			}
			states[i], roles[i] = c.state, c.role
			n.RTTMs[i], n.UpBytes[i], n.DownBytes[i] = c.rttMs, c.up, c.down
			n.TotalUp += c.up
			n.TotalDown += c.down
			if c.rttMs > 0 {
				rttSum += c.rttMs
				rttN++
			}
			if s.uplinkDown {
				continue
			}
			var up bool
			switch c.state {
			case ipc.PoolCellAlive, ipc.PoolCellFlaky:
				up = true
			case ipc.PoolCellDead, ipc.PoolCellParked:
			default:
				continue // unknown / absent: no verdict either way
			}
			observed++
			if up {
				answered++
			}
			if prevUp != nil && *prevUp != up {
				n.Flips++
			}
			prevUp = &up
		}
		n.States, n.Roles = string(states), string(roles)
		if len(roles) > 0 {
			n.Role = string(roles[len(roles)-1])
		}
		if observed > 0 {
			n.UptimePct = 100 * float64(answered) / float64(observed)
		}
		if rttN > 0 {
			n.AvgRTTMs = rttSum / rttN
		}
		if n.Flips > 0 {
			flipped++
		}
		if n.Flips >= 2 {
			dto.Flappers++
		}
		dto.Nodes = append(dto.Nodes, n)
	}
	if len(keys) > 0 {
		dto.ChurnPct = 100 * float64(flipped) / float64(len(keys))
	}

	rank := map[string]int{
		string(ipc.PoolRoleFallback): 0, string(ipc.PoolRoleActive): 1,
		string(ipc.PoolRoleUnranked): 2, string(ipc.PoolRoleIdle): 3, string(ipc.PoolRoleNone): 4,
	}
	sort.Slice(dto.Nodes, func(i, j int) bool {
		a, b := dto.Nodes[i], dto.Nodes[j]
		if rank[a.Role] != rank[b.Role] {
			return rank[a.Role] < rank[b.Role]
		}
		if a.UptimePct != b.UptimePct {
			return a.UptimePct > b.UptimePct
		}
		return a.Name < b.Name
	})
	return dto
}

// renderMasters builds one row per master riding the pool, with the
// outage / demotion counts of events since `since`.
func renderMasters(masters []string, smps []poolSample, events []masterEvent, since time.Time) []ipc.PoolMasterTimelineDTO {
	out := make([]ipc.PoolMasterTimelineDTO, 0, len(masters))
	for _, m := range masters {
		row := ipc.PoolMasterTimelineDTO{Name: m}
		states := make([]byte, len(smps))
		for i, s := range smps {
			c, ok := s.masters[m]
			if !ok {
				c = ipc.MasterCellUnknown
			}
			states[i] = c
		}
		row.States = string(states)
		for _, e := range events {
			if e.master != m || e.at.Before(since) {
				continue
			}
			if e.demotion {
				row.Demotions++
				continue
			}
			row.Outages++
			if e.carrierUp {
				row.OutagesCarrierUp++
			}
			if e.nearSwap {
				row.OutagesNearSwap++
			}
		}
		out = append(out, row)
	}
	return out
}

// noteMasterEdge records a master's outage start (demotion = false) or
// demotion in its pool's timeline and logs what the pool looked like at
// that moment. Called from the latency tracker's edge hooks, which run on
// connection goroutines, so the log line (a store read for node names) is
// written off that path. Names that ride no pool are ignored.
func (s *xraySupervisor) noteMasterEdge(master string, demotion bool) {
	if s == nil {
		return
	}
	carriers, sinceSwap, ok := s.timeline.noteMaster(master, demotion)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		names := poolNodeNamesFrom(ctx, s.xrayStore)
		parts := make([]string, 0, len(carriers))
		for _, c := range carriers {
			name := names[c.key]
			if name == "" {
				name = memberDisplayName(c.key)
			}
			st := map[byte]string{ipc.PoolCellAlive: "up", ipc.PoolCellFlaky: "flaky", ipc.PoolCellDead: "DOWN", ipc.PoolCellUnknown: "unknown"}[c.state]
			if c.rttMs > 0 {
				st += " " + strconv.Itoa(c.rttMs) + "ms"
			}
			parts = append(parts, name+" "+st)
		}
		what := "down"
		if demotion {
			what = "demoted"
		}
		swap := "no shortlist swap since daemon start"
		if sinceSwap >= 0 {
			swap = "shortlist swapped " + sinceSwap.Round(time.Second).String() + " ago"
		}
		s.logger.Printf("xray pool: master %s %s — carrying nodes: %s; %s", master, what, strings.Join(parts, ", "), swap)
	}()
}
