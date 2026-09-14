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

type sample struct {
	rtt   time.Duration
	ok    bool
	at    time.Time
	fails int // consecutive failures; reset by any success
}

// LatencyTracker caches the last probe result per name so a hot path (e.g.
// picking among a multi-outbound binding) can rank candidates by measured
// latency without probing inline. Thread-safe.
type LatencyTracker struct {
	mu      sync.RWMutex
	samples map[string]sample
	ttl     time.Duration // older than this → treated as unknown
}

// NewLatencyTracker returns a tracker whose samples go stale (treated as
// unknown for ranking) after ttl.
func NewLatencyTracker(ttl time.Duration) *LatencyTracker {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &LatencyTracker{samples: map[string]sample{}, ttl: ttl}
}

// Record stores the outcome of one probe of name. A success clears the
// failure streak; a failure extends it (see deadStrikeThreshold).
func (t *LatencyTracker) Record(name string, rtt time.Duration, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.samples[name]
	if ok {
		t.samples[name] = sample{rtt: rtt, ok: true, at: time.Now()}
		return
	}
	t.samples[name] = sample{rtt: s.rtt, ok: false, at: time.Now(), fails: s.fails + 1}
}

// Fail records a failure observed outside the prober — a dial error, or a
// connection that carried bytes one way and heard nothing back. Same
// streak accounting as a failed probe, so real traffic and probes agree on
// when a name is dead.
func (t *LatencyTracker) Fail(name string) { t.Record(name, 0, false) }

// Probe measures target through c and records the result under name,
// returning it.
func (t *LatencyTracker) Probe(ctx context.Context, c Connector, name string, target Target) Result {
	r := Measure(ctx, c, target)
	t.Record(name, r.Latency, r.OK)
	return r
}

// Rank orders names best-first with no incumbent. Equivalent to
// RankFrom(names, "").
func (t *LatencyTracker) Rank(names []string) []string {
	return t.RankFrom(names, "")
}

// RankFrom orders names best-first: healthy (fresh + ok) ascending by
// latency, then unknown/stale (never probed, sample expired, or failing
// but not yet past deadStrikeThreshold), then known-dead last. Stable
// within each tier, so equal-latency or unranked names keep the caller's
// original order. The input slice is not mutated.
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
	now := time.Now()

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
		case fresh && s.ok:
			rs[i] = rankedName{n, i, tierHealthy, s.rtt}
		case fresh && !s.ok && s.fails >= deadStrikeThreshold:
			rs[i] = rankedName{n, i, tierDead, 0}
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
