package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

// Connection health measurement.
//
// Every tuning decision in the proxy path (racing, blame rules, breaker
// thresholds, node parking) was made from log archaeology. This collector
// keeps the numbers that decide whether they work, over a rolling window
// of connStatsWindow one-minute buckets, cheap enough for the per-
// connection path: one mutex, fixed-size histograms, no allocation per
// event beyond the first time an upstream name is seen in a bucket.
//
// Failure causes, each one a distinct thing to fix:
//   - no-upstream: every member of the binding failed to carry it
//   - paused:      tcpHealth refused it (destination recently dead)
//   - dial-ceiling: dialGate was saturated
//   - no-mapping:  a connection to a fake IP with no table entry
//   - no-data:     spliced, sent bytes, never got one back

const (
	connStatsWindow = 15 // one-minute buckets

	causeNoUpstream  = "no-upstream"
	causePaused      = "paused"
	causeDialCeiling = "dial-ceiling"
	causeNoMapping   = "no-mapping"
	causeNoData      = "no-data"
)

// setupBinsMs are the upper bounds of the setup-latency histogram; the
// last bucket is open-ended. Quantiles are reported as a bin's bound.
var setupBinsMs = [...]int64{50, 100, 200, 300, 500, 750, 1000, 1500, 2000, 3000, 5000, 8000, 13000, 20000}

type setupHist [len(setupBinsMs) + 1]int

func (h *setupHist) add(d time.Duration) {
	ms := d.Milliseconds()
	i := sort.Search(len(setupBinsMs), func(i int) bool { return ms <= setupBinsMs[i] })
	h[i]++
}

func (h *setupHist) merge(o *setupHist) {
	for i := range h {
		h[i] += o[i]
	}
}

// quantile returns the bin bound at q (0..1), or -1 with no samples.
func (h *setupHist) quantile(q float64) int64 {
	total := 0
	for _, n := range h {
		total += n
	}
	if total == 0 {
		return -1
	}
	want := int(math.Ceil(q * float64(total))) // nearest rank
	if want < 1 {
		want = 1
	}
	seen := 0
	for i, n := range h {
		seen += n
		if seen >= want {
			if i < len(setupBinsMs) {
				return setupBinsMs[i]
			}
			return setupBinsMs[len(setupBinsMs)-1] + 1
		}
	}
	return -1
}

type upstreamCounts struct {
	carried int // connections it was chosen for
	blamed  int // failures attributed to it (see noteUpstreamFailure)
	noData  int // spliced connections that got nothing back
	setup   setupHist
}

type statBucket struct {
	minute    int64
	conns     int
	ok        int
	failed    map[string]int
	hedges    int
	setup     setupHist
	udpFlows  int
	udpSilent int
	ups       map[string]*upstreamCounts
}

type connStats struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets [connStatsWindow]statBucket
}

func newConnStats() *connStats { return &connStats{now: time.Now} }

// cur returns the bucket for the current minute, recycling a stale one.
// Caller holds mu.
func (c *connStats) cur() *statBucket {
	m := c.now().Unix() / 60
	b := &c.buckets[m%connStatsWindow]
	if b.minute != m {
		*b = statBucket{minute: m, failed: map[string]int{}, ups: map[string]*upstreamCounts{}}
	}
	return b
}

func (b *statBucket) up(name string) *upstreamCounts {
	u := b.ups[name]
	if u == nil {
		u = &upstreamCounts{}
		b.ups[name] = u
	}
	return u
}

// All recorders are safe on a nil receiver.

// established records a connection an upstream agreed to carry, and how
// long the client waited for that.
func (c *connStats) established(name string, setup time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.cur()
	b.conns++
	b.ok++
	b.setup.add(setup)
	u := b.up(name)
	u.carried++
	u.setup.add(setup)
}

// failed records a connection that never reached the splice.
func (c *connStats) failed(cause string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.cur()
	b.conns++
	b.failed[cause]++
}

// noData records a spliced connection that sent and never heard back. It
// was already counted as established; this moves it to the failures.
func (c *connStats) noData(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.cur()
	b.ok--
	b.failed[causeNoData]++
	b.up(name).noData++
}

func (c *connStats) blamed(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur().up(name).blamed++
}

func (c *connStats) hedged() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur().hedges++
}

func (c *connStats) udpFlow(silent bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.cur()
	b.udpFlows++
	if silent {
		b.udpSilent++
	}
}

// connSnapshot is the window's totals.
type connSnapshot struct {
	Conns, OK, Hedges   int
	Failed              map[string]int
	SetupP50, SetupP95  int64
	UDPFlows, UDPSilent int
	Upstreams           []upstreamSnapshot
}

type upstreamSnapshot struct {
	Raw                     string // proxy-store name (what netprobe keys by)
	Name                    string // display name (xray entries without the internal prefix)
	Carried, Blamed, NoData int
	SetupP50                int64
}

func (c *connStats) snapshot() connSnapshot {
	out := connSnapshot{Failed: map[string]int{}}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oldest := c.now().Unix()/60 - connStatsWindow + 1
	var setup setupHist
	ups := map[string]*upstreamCounts{}
	for i := range c.buckets {
		b := &c.buckets[i]
		if b.minute < oldest || b.failed == nil {
			continue
		}
		out.Conns += b.conns
		out.OK += b.ok
		out.Hedges += b.hedges
		out.UDPFlows += b.udpFlows
		out.UDPSilent += b.udpSilent
		for k, n := range b.failed {
			out.Failed[k] += n
		}
		setup.merge(&b.setup)
		for name, u := range b.ups {
			agg := ups[name]
			if agg == nil {
				agg = &upstreamCounts{}
				ups[name] = agg
			}
			agg.carried += u.carried
			agg.blamed += u.blamed
			agg.noData += u.noData
			agg.setup.merge(&u.setup)
		}
	}
	// A no-data outcome lands in the minute it was seen, which can be later
	// than its connection's; once that earlier minute ages out the sum can
	// dip below zero.
	if out.OK < 0 {
		out.OK = 0
	}
	out.SetupP50, out.SetupP95 = setup.quantile(0.5), setup.quantile(0.95)
	for name, u := range ups {
		out.Upstreams = append(out.Upstreams, upstreamSnapshot{
			Raw: name, Name: displayUpstream(name), Carried: u.carried, Blamed: u.blamed, NoData: u.noData,
			SetupP50: u.setup.quantile(0.5),
		})
	}
	sort.Slice(out.Upstreams, func(i, j int) bool {
		if out.Upstreams[i].Carried != out.Upstreams[j].Carried {
			return out.Upstreams[i].Carried > out.Upstreams[j].Carried
		}
		return out.Upstreams[i].Name < out.Upstreams[j].Name
	})
	return out
}

// displayUpstream turns a proxy-store name into what the user named it:
// xray entries live behind hidden "_xray_NAME" proxy rows.
func displayUpstream(name string) string {
	return strings.TrimPrefix(name, xray.InternalProxyName(""))
}
