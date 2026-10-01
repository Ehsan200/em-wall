package netprobe

import (
	"context"
	"sort"
	"sync"
	"time"
)

// deadStrikeThreshold is how many CONSECUTIVE failures a name needs before
// ranking treats it as suspect rather than merely unproven. One failure is
// noise — a probe can lose a race with a network hiccup, and a single
// real connection can end with zero bytes for reasons that have nothing
// to do with the upstream (client abort, immediate 4xx-then-close). Two in
// a row is a path problem. Until then the name sits in the unknown tier:
// behind everything healthy, ahead of everything suspect.
//
// Suspect is an outage in progress, not a verdict: the very next success
// (a probe, or a connection that carried data) restores the name. That is
// what brings back a node that was down and suddenly works — on an
// unstable uplink nodes drop out and return all day, and holding each one
// out for a breaker cooldown after it already works again left sets with
// half their members benched. A node that keeps dropping out is caught by
// the flap rule below instead.
const deadStrikeThreshold = 2

// Circuit breaker.
//
// A consecutive-failure streak only catches an upstream that is dead
// outright. It cannot catch the more common failure in the wild: a node
// that answers four connections out of six and times out the other two.
// Any success resets the streak, so such a node never reaches
// deadStrikeThreshold, keeps its cached latency, and keeps winning the
// ranking — while every connection it drops is a reset the client sees.
// Observed on this daemon: one set member alternating between 12/12 and
// 1/6 to the same destination, chosen for ~15% of that destination's
// connections all day.
//
// So each name also carries a sliding window of recent outcomes, and the
// breaker opens on a failure RATE rather than a streak. Opening is not a
// verdict either: the prober keeps probing an open name in the background
// (it stays in its binding, just ranked last), and the breaker closes
// again once it has been quiet for a cooldown AND the recent record is
// clean. That is what lets a node come back on its own instead of waiting
// for someone to edit the set by hand.
const (
	// breakerWindow is how many recent outcomes are weighed. Ten spans
	// five probe rounds at the daemon's 30s cadence — long enough that one
	// unlucky probe can't open the breaker, short enough that a node which
	// went bad five minutes ago is judged on what it is doing now.
	breakerWindow = 10

	// breakerWindowTTL expires outcomes by age as well as by count, so a
	// name that stops seeing traffic entirely is not held down forever by
	// a handful of ancient failures.
	breakerWindowTTL = 5 * time.Minute

	// breakerMinSamples is the evidence floor. Below it the window says
	// nothing: two failures out of two is a coin toss, not a pattern.
	breakerMinSamples = 4

	// breakerOpenRate / breakerCloseRate are deliberately different —
	// a breaker that opens and closes at the same rate chatters across the
	// threshold, which is the exact flapping this is here to stop. A name
	// must be clearly bad to open and clearly good to close.
	breakerOpenRate  = 0.4
	breakerCloseRate = 0.2

	// breakerCooldown is the minimum time an open breaker stays open the
	// first time it trips. It exists because a single success used to be
	// enough to clear the record: without it, the first probe that happens
	// to land resets the name to healthy and the next connection walks into
	// the same node. Each further trip within breakerTripMemory of the last
	// close doubles it, up to breakerCooldownMax: a node that blipped once
	// is back in half a minute, one that keeps failing stays out longer
	// every time.
	breakerCooldown    = 30 * time.Second
	breakerCooldownMax = 10 * time.Minute
	breakerTripMemory  = 15 * time.Minute

	// flapOutages / flapWindow: a name that enters an outage (a
	// deadStrikeThreshold streak) this many times within flapWindow is
	// flapping — each outage ends quickly, so it never looks flaky by rate,
	// yet every drop-out costs the connections riding it. It opens the
	// breaker like a bad failure rate does.
	flapOutages = 3
	flapWindow  = 10 * time.Minute

	// breakerRelativeMargin: a failure rate only opens the breaker when it
	// is this much worse than the median of the other names. On a lossy
	// uplink every upstream fails a third of its attempts together; an
	// absolute threshold then demoted every member of every set at once
	// (observed), which is the uplink's fault, not theirs, and leaves the
	// ranking with nothing to prefer. The same holds for outages: one that
	// starts while at least half the other names are also down is the
	// uplink, and does not count toward the flap rule.
	breakerRelativeMargin = 0.25

	// resetGrace follows a Reset (network change, wake from sleep): for
	// this long failures are not recorded, only successes. The reset fires
	// as the new network comes up — often before routes and DNS work — and
	// the first probe round used to demote nearly every member at once.
	resetGrace = 15 * time.Second

	// breakerCloseStreak is how many consecutive successes must sit at the
	// end of the window before closing. The rate alone can be satisfied by
	// old successes ageing in; this insists the name works *now*.
	breakerCloseStreak = 2

	// trafficSampleInterval bounds how fast traffic-observed outcomes can
	// enter the window, per name. A client retry storm against one dead
	// destination can produce hundreds of failures a second, and without
	// this the window would be entirely that one destination's opinion of
	// an upstream that carries everything else fine. Streak accounting
	// (deadStrikeThreshold) still counts every one of them, so an upstream
	// that is genuinely down is still killed immediately.
	trafficSampleInterval = 2 * time.Second
)

// BreakerOpenRate is the failure rate that opens the breaker on its own;
// a demotion reported below it was the flap rule.
const BreakerOpenRate = breakerOpenRate

// Hysteresis. Ranking is consulted per connection, so ordering by raw
// latency makes two near-equal upstreams trade places on every probe
// round — and each swap moves the NEXT connection for the same site to a
// different exit IP, which origins read as session hijacking (captcha,
// 403, forced re-login). An incumbent therefore keeps its place unless a
// challenger is better by BOTH a relative and an absolute margin: the
// fraction rejects "180ms vs 185ms" churn, the floor rejects "4ms vs 2ms"
// churn where the fraction alone would fire.
const (
	rankHysteresisFraction = 0.25
	rankHysteresisFloor    = 40 * time.Millisecond
)

// Stability. Healthy names are ranked by an effective cost, not raw RTT: a
// member that answers 40ms faster but dropped a connection a minute ago
// costs more in practice than a slightly slower one that has not failed —
// every drop is a reset the client sees, a probe RTT is a few ms of setup.
// So cost = rtt × (1 + failRateWeight × window failure rate), plus a
// penalty for a recent failure that fades linearly to zero over
// recentFailWindow. Close pings are decided by stability; a much faster
// member still wins. The same cost feeds the hysteresis margins, so an
// incumbent is only displaced by a member that is better all told.
const (
	failRateWeight    = 2.0
	recentFailPenalty = 150 * time.Millisecond
	recentFailWindow  = 10 * time.Minute
)

// outcome is one observation of a name: a probe result, or a real
// connection that did or didn't carry data.
type outcome struct {
	ok bool
	at time.Time
}

type sample struct {
	rtt   time.Duration
	ok    bool
	at    time.Time
	fails int // consecutive failures; reset by any success

	lastFail time.Time // most recent failure, probe or traffic; for stability

	// window holds the last breakerWindow outcomes, oldest first.
	window []outcome
	// lastTraffic is when a traffic-observed outcome last entered the
	// window, for trafficSampleInterval.
	lastTraffic time.Time

	open     bool      // breaker state: open = ranked dead
	openedAt time.Time // when it last opened, for breakerCooldown
	closedAt time.Time // when it last closed, for breakerTripMemory
	trips    int       // opens since the last quiet spell; scales the cooldown

	// outages holds when recent deadStrikeThreshold streaks began, for the
	// flap rule.
	outages []time.Time

	// Real-traffic handshake timing relative to each destination's norm
	// (handshake.go): smoothed log ratio, sample count, last update.
	slow   float64
	slowN  int
	slowAt time.Time
}

// suspect reports an outage in progress: deadStrikeThreshold consecutive
// failures and no success since.
func (s sample) suspect() bool { return s.fails >= deadStrikeThreshold }

// cost is a healthy name's effective ranking cost (see failRateWeight).
func (s sample) cost(rate float64, now time.Time) time.Duration {
	c := time.Duration(float64(s.rtt) * s.handshakeFactor(now) * (1 + failRateWeight*rate))
	if !s.lastFail.IsZero() {
		if age := now.Sub(s.lastFail); age < recentFailWindow {
			c += time.Duration(float64(recentFailPenalty) * float64(recentFailWindow-age) / float64(recentFailWindow))
		}
	}
	return c
}

// cooldown is how long the current open spell lasts at minimum.
func (s sample) cooldown() time.Duration {
	d := breakerCooldown
	for i := 1; i < s.trips && d < breakerCooldownMax; i++ {
		d *= 2
	}
	if d > breakerCooldownMax {
		d = breakerCooldownMax
	}
	return d
}

// LatencyTracker caches the last probe result per name so a hot path (e.g.
// picking among a multi-outbound binding) can rank candidates by measured
// latency without probing inline. It also runs a per-name circuit breaker
// (see the breaker constants) so an intermittently failing upstream is
// demoted and then restored automatically. Thread-safe.
type LatencyTracker struct {
	mu         sync.RWMutex
	samples    map[string]sample
	ttl        time.Duration // older than this → treated as unknown
	now        func() time.Time
	graceUntil time.Time // failures before this are dropped (see resetGrace)
	onTrip     func(name string, open bool, rate float64, samples int)
	onSuspect  func(name string, suspect bool)

	dests     map[string]*destTiming // per-destination handshake baseline (handshake.go)
	destSweep time.Time
}

// NewLatencyTracker returns a tracker whose samples go stale (treated as
// unknown for ranking) after ttl.
func NewLatencyTracker(ttl time.Duration) *LatencyTracker {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &LatencyTracker{samples: map[string]sample{}, ttl: ttl, now: time.Now}
}

// OnBreakerChange registers a callback fired whenever a name's breaker
// opens or closes — the daemon logs it, so a demotion and its recovery are
// both visible without a debugger. Called outside the tracker's lock; safe
// to call anything from it. Pass nil to detach.
func (t *LatencyTracker) OnBreakerChange(fn func(name string, open bool, rate float64, samples int)) {
	t.mu.Lock()
	t.onTrip = fn
	t.mu.Unlock()
}

// OnSuspectChange registers fn to be called when a name enters or leaves
// an outage (see deadStrikeThreshold). Called outside the tracker's lock.
func (t *LatencyTracker) OnSuspectChange(fn func(name string, suspect bool)) {
	t.mu.Lock()
	t.onSuspect = fn
	t.mu.Unlock()
}

// Record stores the outcome of one probe of name. A success clears the
// failure streak; a failure extends it (see deadStrikeThreshold). Probe
// outcomes always enter the breaker window — they are the unbiased
// evidence, one per name per round.
func (t *LatencyTracker) Record(name string, rtt time.Duration, ok bool) {
	t.record(name, rtt, ok, false)
}

// Fail records a failure observed outside the prober — a dial error, or a
// connection that carried bytes one way and heard nothing back. Same
// streak accounting as a failed probe, so real traffic and probes agree on
// when a name is dead; window entry is rate-limited (see
// trafficSampleInterval) so one hot destination can't outvote the probes.
func (t *LatencyTracker) Fail(name string) { t.record(name, 0, false, true) }

// Succeed records a connection that actually carried data through name.
// It is the counterweight to Fail: without it the breaker would hear only
// from failures between probe rounds, and a name doing real work would
// look no better than one sitting idle.
func (t *LatencyTracker) Succeed(name string) { t.record(name, 0, true, true) }

func (t *LatencyTracker) record(name string, rtt time.Duration, ok, fromTraffic bool) {
	t.mu.Lock()
	now := t.now()
	if !ok && now.Before(t.graceUntil) {
		t.mu.Unlock()
		return
	}
	s := t.samples[name]
	wasSuspect := s.suspect()

	// Streak + last-sample bookkeeping. A traffic outcome carries no
	// latency measurement, so it must not overwrite the probed rtt — it
	// says whether the path works, not how fast it is.
	if ok {
		s.ok = true
		s.fails = 0
		if !fromTraffic {
			s.rtt = rtt
		}
	} else {
		s.ok = false
		s.fails++
		s.lastFail = now
	}
	// A traffic outcome shouldn't refresh the sample's freshness either:
	// staleness is about when the name was last MEASURED, and treating a
	// busy-but-unprobed name as fresh would rank it on a stale rtt.
	if !fromTraffic {
		s.at = now
	}

	if !fromTraffic || now.Sub(s.lastTraffic) >= trafficSampleInterval {
		if fromTraffic {
			s.lastTraffic = now
		}
		s.window = appendOutcome(s.window, outcome{ok: ok, at: now}, now)
	} else {
		s.window = trimOutcomes(s.window, now)
	}

	isSuspect := s.suspect()
	switch {
	case isSuspect && !wasSuspect:
		if !t.mostOthersDownLocked(name) {
			s.outages = append(trimTimes(s.outages, now, flapWindow), now)
		}
	case wasSuspect && !isSuspect:
		// The outage is over. Its failures were the streak's to judge, and
		// the streak already did; left in the window they would make a node
		// that just came back look flaky and open the breaker on its first
		// hiccup. Earlier interleaved history stays.
		s.window = clearOutage(s.window)
	}

	was := s.open
	rate, n := failureRate(s.window)
	baseline := 0.0
	if !s.open && rate >= breakerOpenRate {
		baseline = t.peerFailureRateLocked(name)
	}
	s.open = evaluateBreaker(s, rate, n, now, baseline)
	if s.open && !was {
		if !s.closedAt.IsZero() && now.Sub(s.closedAt) > breakerTripMemory {
			s.trips = 0
		}
		s.trips++
		s.openedAt = now
		s.outages = nil // spent: they opened this spell, not the next one
	}
	if was && !s.open {
		s.closedAt = now
	}
	t.samples[name] = s

	hook, shook := t.onTrip, t.onSuspect
	changed := was != s.open
	t.mu.Unlock()

	if isSuspect != wasSuspect && shook != nil {
		shook(name, isSuspect)
	}
	if changed && hook != nil {
		hook(name, s.open, rate, n)
	}
}

// evaluateBreaker returns the breaker state for s given its current window.
// Opening is immediate; closing is deliberately hard (see the constants).
//
// The rate is only weighed outside an outage: during one the window fills
// with the streak's failures, and a node that is simply down would open
// the breaker — and then need a near-clean window to close — when all it
// needs is to answer once.
//
// baseline is the median failure rate of the other names (see
// breakerRelativeMargin); the rate must beat it by the margin as well.
func evaluateBreaker(s sample, rate float64, n int, now time.Time, baseline float64) bool {
	if !s.open {
		if len(trimTimes(s.outages, now, flapWindow)) >= flapOutages {
			return true
		}
		if s.suspect() {
			return false
		}
		return n >= breakerMinSamples && rate >= breakerOpenRate && rate-baseline >= breakerRelativeMargin
	}
	// Open. Stay that way until the cooldown has elapsed, the recent
	// record is clean, and the tail of the window is consecutive success.
	if now.Sub(s.openedAt) < s.cooldown() {
		return true
	}
	if n < breakerMinSamples || rate > breakerCloseRate {
		return true
	}
	return !endsWithSuccesses(s.window, breakerCloseStreak)
}

// appendOutcome adds o to w, dropping entries that aged out or fell off
// the end of the window.
func appendOutcome(w []outcome, o outcome, now time.Time) []outcome {
	w = append(trimOutcomes(w, now), o)
	if len(w) > breakerWindow {
		w = append(w[:0], w[len(w)-breakerWindow:]...)
	}
	return w
}

// trimOutcomes drops outcomes older than breakerWindowTTL. The window is
// ordered oldest-first, so this is a prefix cut.
func trimOutcomes(w []outcome, now time.Time) []outcome {
	cut := 0
	for cut < len(w) && now.Sub(w[cut].at) > breakerWindowTTL {
		cut++
	}
	if cut == 0 {
		return w
	}
	return append(w[:0], w[cut:]...)
}

// mostOthersDownLocked reports whether at least half of the names other
// than self are in an outage right now — the uplink, not self. Needs two
// or more others to say anything. Caller holds t.mu.
func (t *LatencyTracker) mostOthersDownLocked(self string) bool {
	total, down := 0, 0
	for n, s := range t.samples {
		if n == self {
			continue
		}
		total++
		if s.suspect() {
			down++
		}
	}
	return total >= 2 && down*2 >= total
}

// peerFailureRateLocked is the median window failure rate of the names
// other than self that have enough samples to judge. With fewer than two
// other names at all there is nothing to compare against and it is 0 (the
// absolute threshold alone applies). With peers that have not gathered
// enough samples yet it is 1 — no verdict until they have, or a name
// recorded first in each probe round would be judged before its peers.
// Caller holds t.mu.
func (t *LatencyTracker) peerFailureRateLocked(self string) float64 {
	now := t.now()
	var rates []float64
	others := 0
	for n, s := range t.samples {
		if n == self {
			continue
		}
		r, cnt := failureRate(trimmedCopy(s.window, now))
		if cnt == 0 {
			continue // nothing recent: a name no longer probed or used
		}
		others++
		if cnt >= breakerMinSamples {
			rates = append(rates, r)
		}
	}
	if others < 2 {
		return 0
	}
	if len(rates) < 2 {
		return 1
	}
	sort.Float64s(rates)
	return rates[len(rates)/2]
}

// trimmedCopy is w without outcomes older than breakerWindowTTL, never
// modifying w (it belongs to another name's stored sample).
func trimmedCopy(w []outcome, now time.Time) []outcome {
	cut := 0
	for cut < len(w) && now.Sub(w[cut].at) > breakerWindowTTL {
		cut++
	}
	return w[cut:]
}

// clearOutage removes an ended outage's failures from w: the trailing run
// of failures, keeping the success that ended it when that success made it
// into the window (a rate-limited traffic success may not have).
func clearOutage(w []outcome) []outcome {
	var tail []outcome
	end := len(w)
	if end > 0 && w[end-1].ok {
		tail = []outcome{w[end-1]}
		end--
	}
	for end > 0 && !w[end-1].ok {
		end--
	}
	return append(w[:end], tail...)
}

// trimTimes drops timestamps older than span. ts is ordered oldest-first.
// It reslices rather than compacting in place: evaluateBreaker trims a
// copy of the sample, and an in-place shift would corrupt the stored one.
func trimTimes(ts []time.Time, now time.Time, span time.Duration) []time.Time {
	cut := 0
	for cut < len(ts) && now.Sub(ts[cut]) > span {
		cut++
	}
	return ts[cut:]
}

// failureRate reports the share of failures in w and how many outcomes it
// weighed.
func failureRate(w []outcome) (float64, int) {
	if len(w) == 0 {
		return 0, 0
	}
	bad := 0
	for _, o := range w {
		if !o.ok {
			bad++
		}
	}
	return float64(bad) / float64(len(w)), len(w)
}

// endsWithSuccesses reports whether the last n outcomes all succeeded.
func endsWithSuccesses(w []outcome, n int) bool {
	if len(w) < n {
		return false
	}
	for _, o := range w[len(w)-n:] {
		if !o.ok {
			return false
		}
	}
	return true
}

// Probe measures target through c and records the result under name,
// returning it.
func (t *LatencyTracker) Probe(ctx context.Context, c Connector, name string, target Target) Result {
	r := Measure(ctx, c, target)
	t.Record(name, r.Latency, r.OK)
	return r
}

// Reset forgets every name's measurements and breaker state. For a network
// change: latencies and failure windows measured on the previous network
// say nothing about this one, and a breaker opened by the old network's
// outage would otherwise keep a working upstream ranked last until its
// cooldown ran out. Names return as unknown until the next probe round.
//
// Failures are ignored for resetGrace afterwards (see there).
func (t *LatencyTracker) Reset() {
	t.mu.Lock()
	t.samples = make(map[string]sample)
	t.dests = nil
	t.graceUntil = t.now().Add(resetGrace)
	t.mu.Unlock()
}

// Unsettled returns the names that are not currently healthy — never
// measured, stale, failing, suspect or breaker-open — in input order. The
// prober re-checks these between full rounds so a node that starts
// working again is back in its set within seconds, not a full round (plus
// cooldown) later.
func (t *LatencyTracker) Unsettled(names []string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := t.now()
	var out []string
	for _, n := range names {
		s, has := t.samples[n]
		if !has || s.open || !s.ok || now.Sub(s.at) > t.ttl {
			out = append(out, n)
		}
	}
	return out
}

// Health is a name's current standing, for logging and status output.
type Health struct {
	Name        string
	Open        bool // breaker open → ranked behind everything else
	Suspect     bool // outage in progress → ranked behind healthy and unknown
	FailureRate float64
	Samples     int
	RTT         time.Duration
	Fails       int // current consecutive-failure streak
	Since       time.Time
}

// Snapshot returns the current standing of every name the tracker knows.
func (t *LatencyTracker) Snapshot() []Health {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Health, 0, len(t.samples))
	for n, s := range t.samples {
		rate, cnt := failureRate(s.window)
		h := Health{Name: n, Open: s.Open(), Suspect: s.suspect(), FailureRate: rate, Samples: cnt, RTT: s.rtt, Fails: s.fails}
		if s.open {
			h.Since = s.openedAt
		}
		out = append(out, h)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Open reports whether this sample's breaker is open.
func (s sample) Open() bool { return s.open }

// Rank orders names best-first with no incumbent. Equivalent to
// RankFrom(names, "").
func (t *LatencyTracker) Rank(names []string) []string {
	return t.RankFrom(names, "")
}

// RankFrom orders names best-first: healthy (fresh + ok) ascending by
// latency, then unknown/stale (never probed, sample expired, or failing
// but not yet past deadStrikeThreshold), then suspect (an outage in
// progress), then names whose breaker is open last. Healthy and unknown
// are stable, so equal-latency or unranked names keep the caller's
// original order. Suspect and open are ordered least-bad first — lower
// failure rate, then lower last-known latency — because when every member
// is down the best bet is the one that failed least, not the one listed
// first. The input slice is not mutated.
//
// incumbent, when it is one of names and still healthy, is held at the
// front unless the best challenger beats it by the hysteresis margin —
// that is what stops a site's connections from scattering across exits.
// An incumbent that has gone unknown or dead loses its seat immediately.
func (t *LatencyTracker) RankFrom(names []string, incumbent string) []string {
	if len(names) < 2 {
		return names
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := t.now()

	const (
		tierHealthy = 0
		tierUnknown = 1
		tierSuspect = 2
		tierDead    = 3
	)
	rs := make([]rankedName, len(names))
	for i, n := range names {
		s, has := t.samples[n]
		fresh := has && now.Sub(s.at) <= t.ttl
		rate, _ := failureRate(s.window)
		switch {
		case has && s.open:
			// An open breaker outranks every other signal, including a
			// fresh successful probe: the point of the cooldown is that
			// one good measurement does not undo the demotion.
			rs[i] = rankedName{n, i, tierDead, s.rtt, rate}
		case has && s.suspect():
			rs[i] = rankedName{n, i, tierSuspect, s.rtt, rate}
		case fresh && s.ok:
			rs[i] = rankedName{n, i, tierHealthy, s.cost(rate, now), 0}
		default:
			rs[i] = rankedName{n, i, tierUnknown, 0, 0}
		}
	}
	sort.SliceStable(rs, func(a, b int) bool {
		ra, rb := rs[a], rs[b]
		if ra.tier != rb.tier {
			return ra.tier < rb.tier
		}
		switch ra.tier {
		case tierHealthy:
			if ra.rtt != rb.rtt {
				return ra.rtt < rb.rtt
			}
		case tierSuspect, tierDead:
			if ra.rate != rb.rate {
				return ra.rate < rb.rate
			}
			// A name that has never answered (rtt 0) goes after any that has.
			if (ra.rtt == 0) != (rb.rtt == 0) {
				return rb.rtt == 0
			}
			if ra.rtt != rb.rtt {
				return ra.rtt < rb.rtt
			}
		}
		return ra.idx < rb.idx
	})

	// Hysteresis: a healthy incumbent stays first unless the leader is
	// better by both margins.
	if incumbent != "" && rs[0].name != incumbent {
		for i, r := range rs {
			if r.name != incumbent || r.tier != tierHealthy {
				continue
			}
			if !beatsByMargin(rs[0], r) {
				copy(rs[1:i+1], rs[:i])
				rs[0] = r
			}
			break
		}
	}

	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.name
	}
	return out
}

// rankedName is one candidate during a RankFrom pass.
type rankedName struct {
	name string
	idx  int // original position, for stable tie-breaks
	tier int
	rtt  time.Duration // healthy: effective cost (see sample.cost); else last RTT
	rate float64       // window failure rate; orders the suspect and dead tiers
}

// beatsByMargin reports whether challenger is enough better than incumbent
// to justify moving traffic. A challenger in a better tier always wins.
func beatsByMargin(challenger, incumbent rankedName) bool {
	if challenger.tier != incumbent.tier {
		return challenger.tier < incumbent.tier
	}
	gain := incumbent.rtt - challenger.rtt
	return gain >= rankHysteresisFloor && float64(gain) >= rankHysteresisFraction*float64(incumbent.rtt)
}
