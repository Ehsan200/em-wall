package main

import (
	"slices"
	"sort"
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
// Observation only. Nothing reads it to make a decision yet. In memory; a
// daemon restart starts a fresh history.

const (
	nodeTimelineInterval = 10 * time.Second
	nodeTimelineSpan     = 30 * time.Minute
	nodeTimelineCap      = int(nodeTimelineSpan / nodeTimelineInterval)
)

type nodeCell struct {
	state, role byte
	rttMs       int
	up, down    int64
}

type poolSample struct {
	at         time.Time
	uplinkDown bool
	nodes      map[string]nodeCell // member key → cell; parked members included
}

type poolHistory struct {
	masters []string
	samples []poolSample // oldest first, at most nodeTimelineCap
}

// byteCount is one outbound's cumulative counters.
type byteCount struct{ up, down int64 }

// poolTimeline holds the history of every loaded pool, keyed by the slot's
// owning master. Safe on a nil receiver, which records nothing.
type poolTimeline struct {
	mu    sync.Mutex
	now   func() time.Time
	pools map[string]*poolHistory
	last  map[string]byteCount // outbound tag → counters at the previous sample
}

func newPoolTimeline() *poolTimeline {
	return &poolTimeline{now: time.Now, pools: map[string]*poolHistory{}, last: map[string]byteCount{}}
}

// record folds one round into the history. slots are the members loaded in
// xray right now (parked ones absent); traffic is xray's per-outbound
// cumulative counters by tag.
func (t *poolTimeline) record(slots []xray.DialerSlot, byTag map[string]nodeStatus, traffic map[string]byteCount, parked map[string]bool) {
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
		smp := poolSample{at: now, nodes: make(map[string]nodeCell, len(slot.Members))}
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
	}
	for m := range t.pools {
		if !live[m] {
			delete(t.pools, m)
		}
	}
	t.last = nextLast
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
		out = append(out, renderPool(owner, h.masters, smps, names))
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
		Nodes:       []ipc.PoolNodeTimelineDTO{},
	}
	keys := map[string]bool{}
	for i, s := range smps {
		dto.Times[i] = s.at.Unix()
		dto.UplinkDown[i] = s.uplinkDown
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
