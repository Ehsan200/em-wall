package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// Pool switch strategies (core/xray/strategy.go), ported from em-xray.
//
// stable  — today's behaviour: smoothed shortlist (xray_shortlist.go),
//           sticky pair, parking.
// agile   — for pools whose nodes come and go in waves. No parking (a pool
//           turning agile gets its parked nodes back at once) and no
//           smoothing: every nodeTimelineInterval the balancer is pointed at
//           the members answering right now, fastest first, up to
//           agileActiveMax, with the next live one as fallback. A carrying
//           member that stops answering has its masters' open connections
//           marked stale, and the stale-path sweeper closes the ones that
//           actually stalled, so apps reconnect onto a live node within
//           seconds instead of hanging until connIdle. (em-xray destroys the
//           sockets to the node's server; macOS has no ss -K, and the
//           daemon owns both ends of every proxied connection anyway.)
// manual  — the pool is loaded with the pinned nodes only. Traffic never
//           leaves them; no parking. Unpinning removes the outbound, and
//           the live apply's stale-path pass moves stalled streams off it.
// auto    — the default. Stable until the pool's health timeline shows it
//           flapping, agile from then until it has been calm a while.
//
// A pool drawing on several subscriptions follows manual if any of them is
// manual (pins are an explicit order), else the most aggressive: agile if
// any is set agile, else the classifier's call if any is on auto, else
// stable. A pool of entries only (no subscription) is auto.

// Agile selection.
const (
	agileActiveMax = 4 // an agile pool spreads over at most this many live members

	// An incumbent's cost is divided by agileStickyRatio and lowered by
	// agileStickyFloor, so RTT noise alone doesn't rewrite routing every
	// poll; a clearly faster node still takes the seat.
	agileStickyRatio = 1.25
	agileStickyFloor = 50 * time.Millisecond
)

// Auto classifier thresholds (em-xray's DefaultAutoTuning). A pool is
// flapping when, over autoWindow, its whole pick died at once
// autoPickLosses times, or autoFlappingPct of its observed nodes (and at
// least autoMinFlapping) flipped up↔down autoFlips times or more. An agile
// pool goes back to stable after autoCalm without flapping — slow on
// purpose, so a pool on the edge doesn't bounce.
const (
	autoWindow      = 10 * time.Minute
	autoFlips       = 2
	autoFlappingPct = 30
	autoMinFlapping = 2
	autoPickLosses  = 2
	autoCalm        = 30 * time.Minute
)

// ---- strategy resolution ----

// poolStrategy is the effective switch strategy of a pool drawing on refs,
// and whether the auto classifier chose it.
func (s *xraySupervisor) poolStrategy(ctx context.Context, poolKey string, refs []xray.DialerRef) (string, bool) {
	auto, agile, subs := false, false, 0
	for _, r := range refs {
		if r.Kind != xray.DialerKindXraysub || s.xrayStore == nil {
			continue
		}
		sub, err := s.xrayStore.GetSubByName(ctx, r.Name)
		if err != nil {
			continue
		}
		subs++
		switch sub.EffectiveStrategy() {
		case xray.StrategyManual:
			return xray.StrategyManual, false
		case xray.StrategyAgile:
			agile = true
		case xray.StrategyAuto:
			auto = true
		}
	}
	switch {
	case agile:
		return xray.StrategyAgile, false
	case subs > 0 && !auto:
		return xray.StrategyStable, false
	case s.auto.agile(poolKey):
		return xray.StrategyAgile, true
	}
	return xray.StrategyStable, true
}

// poolPins returns the member keys pinned by the manual subscriptions
// among refs, in member order.
func (s *xraySupervisor) poolPins(ctx context.Context, refs []xray.DialerRef, members []xray.DialerMember) []string {
	pinned := map[string]bool{}
	for _, r := range refs {
		if r.Kind != xray.DialerKindXraysub || s.xrayStore == nil {
			continue
		}
		sub, err := s.xrayStore.GetSubByName(ctx, r.Name)
		if err != nil || sub.EffectiveStrategy() != xray.StrategyManual {
			continue
		}
		fps, err := s.xrayStore.PinnedFingerprints(ctx, sub.ID)
		if err != nil {
			continue
		}
		for fp := range fps {
			pinned[fp] = true
		}
	}
	var out []string
	for _, m := range members {
		if pinned[m.Key] {
			out = append(out, m.Key)
		}
	}
	return out
}

// onlyKeys keeps the members named in keys.
func onlyKeys(members []xray.DialerMember, keys []string) []xray.DialerMember {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	out := make([]xray.DialerMember, 0, len(keys))
	for _, m := range members {
		if want[m.Key] {
			out = append(out, m)
		}
	}
	return out
}

func orStable(strategy string) string {
	if strategy == "" {
		return xray.StrategyStable
	}
	return strategy
}

// logStrategyChanges logs pools whose strategy differs from the loaded one
// (auto switches log themselves, with their reason).
func (s *xraySupervisor) logStrategyChanges(old, cur []xray.DialerSlot) {
	was := map[string]string{}
	for _, sl := range old {
		was[sl.PoolKey] = orStable(sl.Strategy)
	}
	for _, sl := range cur {
		if prev, ok := was[sl.PoolKey]; ok && prev != orStable(sl.Strategy) && !sl.Auto {
			s.logger.Printf("xray pool %s: strategy %s → %s", sl.Master, prev, orStable(sl.Strategy))
		}
	}
}

// ---- agile pick ----

type agilePick struct {
	active []string // carrying, sorted by key: the balancer picks among them at random, so order is noise
	spare  string   // fallback: the next live member, else the fastest active one
}

// agilePicker holds each agile pool's current pick, by pool key. Nil-safe.
type agilePicker struct {
	mu    sync.Mutex
	picks map[string]agilePick
}

func newAgilePicker() *agilePicker { return &agilePicker{picks: map[string]agilePick{}} }

func (p *agilePicker) get(poolKey string) (agilePick, bool) {
	if p == nil {
		return agilePick{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pk, ok := p.picks[poolKey]
	return pk, ok
}

// agileChange is one agile pool whose pick moved.
type agileChange struct {
	slot    xray.DialerSlot
	from    agilePick
	to      agilePick
	dropped []string // formerly carrying members that stopped answering
}

// observe re-picks every loaded agile pool from one observatory round and
// returns the pools whose pick changed. Non-agile pools are forgotten.
func (p *agilePicker) observe(slots []xray.DialerSlot, byTag map[string]nodeStatus) []agileChange {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	live := map[string]bool{}
	var changes []agileChange
	for _, sl := range slots {
		if !sl.Agile() {
			continue
		}
		live[sl.PoolKey] = true
		old := p.picks[sl.PoolKey]
		next, ok := pickAgile(sl, byTag, old)
		if !ok {
			continue // nothing measured yet, or the uplink is down: keep the pick
		}
		if equalStrings(next.active, old.active) && next.spare == old.spare {
			continue
		}
		p.picks[sl.PoolKey] = next
		ch := agileChange{slot: sl, from: old, to: next}
		for _, k := range old.active {
			if !agileAlive(byTag[xray.SlotMemberTag(sl.Index, k)]) {
				ch.dropped = append(ch.dropped, k)
			}
		}
		changes = append(changes, ch)
	}
	for k := range p.picks {
		if !live[k] {
			delete(p.picks, k)
		}
	}
	return changes
}

// agileAlive: the node answers often enough that xray's own balancer would
// still use it (SlotBalancerTolerance of its ping window).
func agileAlive(st nodeStatus) bool {
	hp := st.HealthPing
	return st.Alive && hp.All > 0 && float64(hp.Fail)/float64(hp.All) <= xray.SlotBalancerTolerance
}

// pickAgile is an agile pool's pick: every member alive in the latest
// observatory round, cheapest first by its last RTT (inflated by the
// window's failure rate), up to agileActiveMax, then the spare — the next
// live member. Incumbents get a sticky discount. ok is false when no member
// is alive, or none has been measured: every node down at once is the
// uplink, and the current pick is kept for when it returns.
func pickAgile(sl xray.DialerSlot, byTag map[string]nodeStatus, old agilePick) (agilePick, bool) {
	wasActive := map[string]bool{}
	for _, k := range old.active {
		wasActive[k] = true
	}
	type cand struct {
		key  string
		cost time.Duration
	}
	var alive []cand
	for _, m := range sl.Members {
		st, ok := byTag[xray.SlotMemberTag(sl.Index, m.Key)]
		if !ok || !agileAlive(st) {
			continue
		}
		rtt := time.Duration(st.Delay) * time.Millisecond
		if rtt <= 0 {
			rtt = time.Duration(st.HealthPing.Average)
		}
		rate := float64(st.HealthPing.Fail) / float64(st.HealthPing.All)
		cost := time.Duration(float64(rtt) * (1 + shortlistFailWeight*rate))
		if wasActive[m.Key] {
			cost = time.Duration(float64(cost)/agileStickyRatio) - agileStickyFloor
		}
		alive = append(alive, cand{key: m.Key, cost: cost})
	}
	if len(alive) == 0 {
		return agilePick{}, false
	}
	// Members the path prober caught failing the real path go, unless
	// nothing else is alive (see dropBroken).
	if len(sl.Broken) > 0 {
		var ok []cand
		for _, c := range alive {
			if !slices.Contains(sl.Broken, c.key) {
				ok = append(ok, c)
			}
		}
		if len(ok) > 0 {
			alive = ok
		}
	}
	sort.SliceStable(alive, func(i, j int) bool {
		if alive[i].cost != alive[j].cost {
			return alive[i].cost < alive[j].cost
		}
		return alive[i].key < alive[j].key
	})
	n := min(len(alive), agileActiveMax)
	out := agilePick{active: make([]string, 0, n), spare: alive[0].key}
	for _, c := range alive[:n] {
		out.active = append(out.active, c.key)
	}
	sort.Strings(out.active)
	switch {
	case len(alive) > n:
		out.spare = alive[n].key
	case slices.Contains(out.active, old.spare):
		out.spare = old.spare // no spare to hand: keep the fallback while it lives
	}
	return out, true
}

// ---- auto classifier ----

type autoState struct {
	agile      bool
	since      time.Time // when the current mode began
	lastFlappy time.Time // latest round that judged the pool flapping
	reason     string    // why the current mode was chosen
}

// autoClassifier keeps each auto pool's mode by pool key. Nil-safe. In
// memory: a daemon restart starts every auto pool stable again.
type autoClassifier struct {
	mu    sync.Mutex
	now   func() time.Time
	pools map[string]*autoState
}

func newAutoClassifier() *autoClassifier {
	return &autoClassifier{now: time.Now, pools: map[string]*autoState{}}
}

func (c *autoClassifier) agile(poolKey string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[poolKey]
	return st != nil && st.agile
}

func (c *autoClassifier) state(poolKey string) (autoState, bool) {
	if c == nil {
		return autoState{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.pools[poolKey]
	if !ok {
		return autoState{}, false
	}
	return *st, true
}

type autoEvent struct {
	poolKey string
	agile   bool
	reason  string
}

// flapping judges one pool's window against the thresholds.
func flapping(p ipc.PoolTimelineDTO) (bool, string) {
	if p.PickLosses >= autoPickLosses {
		return true, fmt.Sprintf("whole pick died %d× in %s", p.PickLosses, shortMinutes(autoWindow))
	}
	observed, flappers := 0, 0
	for _, n := range p.Nodes {
		if !strings.ContainsAny(n.States, "AFDP") {
			continue // never measured in the window
		}
		observed++
		if n.Flips >= autoFlips {
			flappers++
		}
	}
	if flappers >= autoMinFlapping && flappers*100 >= autoFlappingPct*observed {
		return true, fmt.Sprintf("%d/%d nodes flipped up↔down %d+ times in %s", flappers, observed, autoFlips, shortMinutes(autoWindow))
	}
	return false, ""
}

// evaluate judges the auto pools from their timeline windows (keyed by
// pool key) and returns the switches it made. Pools no longer auto are
// forgotten.
func (c *autoClassifier) evaluate(windows map[string]ipc.PoolTimelineDTO, auto map[string]bool) []autoEvent {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k := range c.pools {
		if !auto[k] {
			delete(c.pools, k)
		}
	}
	keys := make([]string, 0, len(windows))
	for k := range windows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var events []autoEvent
	for _, k := range keys {
		if !auto[k] {
			continue
		}
		st := c.pools[k]
		if st == nil {
			st = &autoState{since: now, reason: "no flapping seen"}
			c.pools[k] = st
		}
		flappy, why := flapping(windows[k])
		switch {
		case flappy:
			st.lastFlappy = now
			if !st.agile {
				st.agile, st.since, st.reason = true, now, why
				events = append(events, autoEvent{poolKey: k, agile: true, reason: why})
			}
		case st.agile && now.Sub(st.lastFlappy) >= autoCalm:
			why := "calm for " + shortMinutes(autoCalm)
			st.agile, st.since, st.reason = false, now, why
			events = append(events, autoEvent{poolKey: k, reason: why})
		}
	}
	return events
}

func shortMinutes(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// ---- supervisor glue ----

// autoSwitched judges the loaded auto pools and reports whether any changed
// mode (the caller reconciles to apply it).
func (s *xraySupervisor) autoSwitched(slots []xray.DialerSlot) bool {
	auto := map[string]bool{}
	byOwner := map[string]xray.DialerSlot{}
	for _, sl := range slots {
		if sl.Auto {
			auto[sl.PoolKey] = true
		}
		byOwner[sl.Master] = sl
	}
	if len(auto) == 0 {
		s.auto.evaluate(nil, nil) // forget pools that left auto
		return false
	}
	windows := map[string]ipc.PoolTimelineDTO{}
	for _, p := range s.timeline.snapshot("", autoWindow, nil) {
		if sl, ok := byOwner[p.Master]; ok {
			windows[sl.PoolKey] = p
		}
	}
	events := s.auto.evaluate(windows, auto)
	for _, e := range events {
		mode := xray.StrategyStable
		if e.agile {
			mode = xray.StrategyAgile
		}
		master := e.poolKey
		for _, sl := range slots {
			if sl.PoolKey == e.poolKey {
				master = sl.Master
			}
		}
		s.logger.Printf("xray pool %s: auto → %s (%s)", master, mode, e.reason)
	}
	return len(events) > 0
}

// agileStep re-picks the agile pools from this round, marks the open
// connections of masters whose carrying member stopped answering as stale
// (the sweeper closes the stalled ones), and reports whether any pick moved.
func (s *xraySupervisor) agileStep(slots []xray.DialerSlot, byTag map[string]nodeStatus) bool {
	changes := s.agile.observe(slots, byTag)
	for _, c := range changes {
		if len(c.dropped) == 0 {
			continue
		}
		masters := map[string]bool{}
		for _, m := range c.slot.SlotMasters() {
			masters[m] = true
		}
		n := s.live.markStale(masters)
		s.logger.Printf("xray pool %s (agile): carrying node(s) %s stopped answering — %d open connection(s) will be closed if stalled",
			c.slot.Master, strings.Join(c.dropped, ", "), n)
	}
	return len(changes) > 0
}

type strategyView struct {
	strategy string
	auto     bool
	reason   string
	since    time.Time
}

// poolStrategyViews describes each loaded pool's strategy for the health
// view, keyed by owning master.
func (s *xraySupervisor) poolStrategyViews() map[string]strategyView {
	out := map[string]strategyView{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	slots := s.loadedSlots
	s.mu.Unlock()
	for _, sl := range slots {
		v := strategyView{strategy: orStable(sl.Strategy), auto: sl.Auto}
		if v.auto {
			if st, ok := s.auto.state(sl.PoolKey); ok {
				v.reason, v.since = st.reason, st.since
			}
		}
		out[sl.Master] = v
	}
	return out
}
