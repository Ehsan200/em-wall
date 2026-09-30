package netprobe

import (
	"math"
	"time"
)

// Handshake timing from real traffic.
//
// Probes measure each name every 30s against one fixed target. Real
// connections measure it constantly — the time from sending the client's
// opening (a TLS ClientHello) to the first byte back is a full round trip
// through the upstream to a real destination — but the raw figure can't
// rank names: a name carrying a far-away site looks slow next to one
// carrying a nearby CDN, and sticky selection means members carry
// different sites. So each timing is compared with the recent timing of the
// same destination through any name, and only that ratio is kept per name:
// "40% slower than usual for wherever it went". A name that is quick on
// its probe but drags on real traffic ends up costing more than its probe
// says, and the reverse.
//
// The ratio is kept in log space (so 2× slower and 2× faster weigh the
// same), smoothed per name, and scales the name's probe RTT in sample.cost
// by exp(handshakeWeight × ratio) — damped, because a single site's timing
// is noisy and the probe stays the primary signal. It is ignored until a
// name has handshakeMinSamples and once it is older than handshakeTTL.
const (
	handshakeWeight     = 0.5
	handshakeMaxLog     = 2.2  // ratio clamp: e^2.2 ≈ 9× slower…
	handshakeMinLog     = -1.4 // …e^-1.4 ≈ 4× faster; factor stays in [0.5, 3]
	handshakeNameAlpha  = 0.2  // per-name smoothing of the ratio
	handshakeDestAlpha  = 0.1  // per-destination baseline smoothing
	handshakeMinSamples = 3
	handshakeTTL        = 15 * time.Minute
	handshakeDestTTL    = 30 * time.Minute
	handshakeSweepEvery = time.Minute
)

// destTiming is one destination's recent handshake baseline, over every
// name that carried it.
type destTiming struct {
	ewma  float64 // log seconds
	last  string  // name of the latest sample
	mixed bool    // two or more names have carried it: a ratio means something
	at    time.Time
}

// ObserveHandshake records that name took d to answer a client's opening
// for dest (a site or host key). d may be a lower bound — a raced attempt
// still waiting when a sibling won — which only understates slowness.
func (t *LatencyTracker) ObserveHandshake(name, dest string, d time.Duration) {
	if name == "" || dest == "" || d <= 0 {
		return
	}
	x := math.Log(d.Seconds())
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.dests == nil {
		t.dests = map[string]*destTiming{}
	}
	t.sweepDestsLocked(now)
	ds := t.dests[dest]
	if ds == nil {
		t.dests[dest] = &destTiming{ewma: x, last: name, at: now}
		return
	}
	if name != ds.last {
		ds.mixed = true
	}
	// Only names the tracker already knows: a name that was never probed
	// has no RTT to scale, and a sample created here would count as a
	// live peer in the uplink-outage checks.
	if s, has := t.samples[name]; ds.mixed && has {
		r := math.Max(handshakeMinLog, math.Min(handshakeMaxLog, x-ds.ewma))
		if s.slowN == 0 || now.Sub(s.slowAt) > handshakeTTL {
			s.slow, s.slowN = r, 0
		} else {
			s.slow += handshakeNameAlpha * (r - s.slow)
		}
		s.slowN++
		s.slowAt = now
		t.samples[name] = s
	}
	ds.ewma += handshakeDestAlpha * (x - ds.ewma)
	ds.last = name
	ds.at = now
}

func (t *LatencyTracker) sweepDestsLocked(now time.Time) {
	if now.Sub(t.destSweep) < handshakeSweepEvery {
		return
	}
	t.destSweep = now
	for k, ds := range t.dests {
		if now.Sub(ds.at) > handshakeDestTTL {
			delete(t.dests, k)
		}
	}
}

// handshakeFactor is the multiplier traffic timing applies to s's probe
// RTT: 1 without enough recent evidence.
func (s sample) handshakeFactor(now time.Time) float64 {
	if s.slowN < handshakeMinSamples || now.Sub(s.slowAt) > handshakeTTL {
		return 1
	}
	return math.Exp(handshakeWeight * math.Max(handshakeMinLog, math.Min(handshakeMaxLog, s.slow)))
}
