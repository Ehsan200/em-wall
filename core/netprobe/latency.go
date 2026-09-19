package netprobe

import (
	"context"
	"sort"
	"sync"
	"time"
)

// deadStrikeThreshold is how many CONSECUTIVE failures a name needs before
// ranking treats it as dead rather than merely unproven. One failure is
// noise — a probe can lose a race with a network hiccup, and a single
// real connection can end with zero bytes for reasons that have nothing
// to do with the upstream (client abort, immediate 4xx-then-close). Two in
// a row is a path problem. Until then the name sits in the unknown tier:
// behind everything healthy, ahead of everything dead.
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

	// breakerCooldown is the minimum time an open breaker stays open. It
	// exists because a single success used to be enough to clear the
	// record: without it, the first probe that happens to land resets the
	// name to healthy and the next connection walks into the same node.
	breakerCooldown = 60 * time.Second

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

	// window holds the last breakerWindow outcomes, oldest first.
	window []outcome
	// lastTraffic is when a traffic-observed outcome last entered the
	// window, for trafficSampleInterval.
	lastTraffic time.Time

	open     bool      // breaker state: open = ranked dead
	openedAt time.Time // when it last opened, for breakerCooldown
}

// LatencyTracker caches the last probe result per name so a hot path (e.g.
// picking among a multi-outbound binding) can rank candidates by measured
// latency without probing inline. It also runs a per-name circuit breaker
// (see the breaker constants) so an intermittently failing upstream is
// demoted and then restored automatically. Thread-safe.
type LatencyTracker struct {
	mu      sync.RWMutex
	samples map[string]sample
	ttl     time.Duration // older than this → treated as unknown
	now     func() time.Time
	onTrip  func(name string, open bool, rate float64, samples int)
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
	s := t.samples[name]

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

	was := s.open
	rate, n := failureRate(s.window)
	s.open = evaluateBreaker(s, rate, n, now)
	if s.open && !was {
		s.openedAt = now
	}
	t.samples[name] = s

	hook := t.onTrip
	changed := was != s.open
	t.mu.Unlock()

	if changed && hook != nil {
		hook(name, s.open, rate, n)
	}
}

// evaluateBreaker returns the breaker state for s given its current window.
// Opening is immediate; closing is deliberately hard (see the constants).
func evaluateBreaker(s sample, rate float64, n int, now time.Time) bool {
	if !s.open {
		if s.fails >= deadStrikeThreshold {
			return true
		}
		return n >= breakerMinSamples && rate >= breakerOpenRate
	}
	// Open. Stay that way until the cooldown has elapsed, the recent
	// record is clean, and the tail of the window is consecutive success.
	if now.Sub(s.openedAt) < breakerCooldown {
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

// Health is a name's current standing, for logging and status output.
type Health struct {
	Name        string
	Open        bool // breaker open → ranked behind everything else
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
		h := Health{Name: n, Open: s.Open(), FailureRate: rate, Samples: cnt, RTT: s.rtt, Fails: s.fails}
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
// but not yet past deadStrikeThreshold), then names whose breaker is open
// last. Stable within each tier, so equal-latency or unranked names keep
// the caller's original order. The input slice is not mutated.
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
		tierDead    = 2
	)
	rs := make([]rankedName, len(names))
	for i, n := range names {
		s, has := t.samples[n]
		fresh := has && now.Sub(s.at) <= t.ttl
		switch {
		case has && s.open:
			// An open breaker outranks every other signal, including a
			// fresh successful probe: the point of the cooldown is that
			// one good measurement does not undo the demotion.
			rs[i] = rankedName{n, i, tierDead, 0}
		case fresh && s.ok:
			rs[i] = rankedName{n, i, tierHealthy, s.rtt}
		default:
			rs[i] = rankedName{n, i, tierUnknown, 0}
		}
	}
	sort.SliceStable(rs, func(a, b int) bool {
		if rs[a].tier != rs[b].tier {
			return rs[a].tier < rs[b].tier
		}
		if rs[a].tier == tierHealthy && rs[a].rtt != rs[b].rtt {
			return rs[a].rtt < rs[b].rtt
		}
		return rs[a].idx < rs[b].idx
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
	rtt  time.Duration
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
