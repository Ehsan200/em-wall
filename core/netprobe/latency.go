package netprobe

import (
	"context"
	"sort"
	"sync"
	"time"
)

type sample struct {
	rtt time.Duration
	ok  bool
	at  time.Time
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

// Record stores the outcome of one probe of name.
func (t *LatencyTracker) Record(name string, rtt time.Duration, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples[name] = sample{rtt: rtt, ok: ok, at: time.Now()}
}

// Probe measures target through c and records the result under name,
// returning it.
func (t *LatencyTracker) Probe(ctx context.Context, c Connector, name string, target Target) Result {
	r := Measure(ctx, c, target)
	t.Record(name, r.Latency, r.OK)
	return r
}

// Rank orders names best-first: healthy (fresh + ok) ascending by latency,
// then unknown/stale (never probed or sample expired), then known-dead
// last. Stable within each tier, so equal-latency or unranked names keep the
// caller's original order. The input slice is not mutated.
func (t *LatencyTracker) Rank(names []string) []string {
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
	type ranked struct {
		name string
		idx  int
		tier int
		rtt  time.Duration
	}
	rs := make([]ranked, len(names))
	for i, n := range names {
		s, has := t.samples[n]
		fresh := has && now.Sub(s.at) <= t.ttl
		switch {
		case fresh && s.ok:
			rs[i] = ranked{n, i, tierHealthy, s.rtt}
		case fresh && !s.ok:
			rs[i] = ranked{n, i, tierDead, 0}
		default:
			rs[i] = ranked{n, i, tierUnknown, 0}
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
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.name
	}
	return out
}
