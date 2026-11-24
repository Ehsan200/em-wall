package main

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

// Which pool node is carrying a master right now.
//
// The slot balancer picks a member per connection, and nothing in xray's
// API reports the pick: `api bi` names the balancer's ranking, the byte
// counters only say who moved data in the last 10 s sample. xray's access
// log does — one line per routing decision, logged after the balancer ran,
// so the outbound it names is the member actually dialed. Verified against
// the real binary (TestAccessLogNamesBalancerPick):
//
//	2026/10/24 11:45:02.484253 from tcp:127.0.0.1:58821 accepted tcp:127.0.0.1:47199 [slot0-in -> slot0-out-b]
//	2026/10/24 11:45:19.297057 from tcp:127.0.0.1:58995 accepted tcp:127.0.0.1:47199 [in-z >> block]
//	2026/10/24 11:45:19.341125 from udp:127.0.0.1:62056 accepted udp:127.0.0.1:47198 [in-r -> out-y]
//	2026/10/24 11:45:19.311579 from tcp:127.0.0.1:58996 rejected  proxy/socks: invalid username or password
//
// "->" is a rule match, ">>" the default outbound (no rule matched); xray
// also writes "==>" for a route picked by an inbound's sniffing override.
// Destinations may be IPv6 ("tcp:[::1]:443") but never contain a space.
// Observatory pings and dialerProxy hops don't pass the dispatcher and
// never appear; a master's own connection shows twice — "[in-M -> out-M]"
// for the client side, "[slotN-in -> slotN-out-KEY]" for its transport to
// the server, which is the line that names the node.
//
// The tailer (xray_access_tail.go) feeds every accepted line through
// observe; this file keeps, per inbound tag, a rolling count of picks per
// outbound tag. Observation only: nothing on the connection path reads it,
// and memory is bounded (accessMaxRoutes × accessMaxOutbounds counters).

const (
	// accessWindow is the finest history kept: per-second pick counts.
	accessWindow = 60 * time.Second
	// accessRecent is the "now" sub-window: Active / Dominant / the map's
	// pool hop look only at it.
	accessRecent = 30 * time.Second
	// Pool members also keep per-bucket counts over the pool timeline's
	// span, so the Health tab can mark which cells a node was picked in.
	accessHistBucket = nodeTimelineInterval
	accessHistSpan   = nodeTimelineSpan
	accessHistCap    = int(accessHistSpan / accessHistBucket)

	accessMaxRoutes    = 256 // inbound tags tracked
	accessMaxOutbounds = 256 // outbound tags per inbound
	// accessDominance: the recent share above which one member is said to
	// carry the pool, with at least accessDominanceMin picks behind it.
	accessDominance    = 0.8
	accessDominanceMin = 3
)

const accessWindowSlots = int(accessWindow / time.Second)

// pickCounter counts one outbound's picks for one inbound. Buckets are
// stamped with the time unit they hold, so a stale bucket reads as zero
// without anything sweeping it.
type pickCounter struct {
	last   time.Time
	secs   [accessWindowSlots]uint32
	secAt  [accessWindowSlots]int64 // unix second held by secs[i]
	hist   []uint32                 // per accessHistBucket; nil unless a slot route
	histAt []int64
}

func (c *pickCounter) add(t time.Time, withHist bool) {
	if t.After(c.last) {
		c.last = t
	}
	s := t.Unix()
	i := int(s % int64(accessWindowSlots))
	if c.secAt[i] != s {
		c.secAt[i], c.secs[i] = s, 0
	}
	c.secs[i]++
	if !withHist {
		return
	}
	if c.hist == nil {
		c.hist, c.histAt = make([]uint32, accessHistCap), make([]int64, accessHistCap)
	}
	b := s / int64(accessHistBucket/time.Second)
	j := int(b % int64(accessHistCap))
	if c.histAt[j] != b {
		c.histAt[j], c.hist[j] = b, 0
	}
	c.hist[j]++
}

// count sums the picks in the last d (≤ accessWindow) before now.
func (c *pickCounter) count(now time.Time, d time.Duration) int {
	from := now.Add(-d).Unix() // exclusive
	to := now.Unix()
	n := 0
	for i, at := range c.secAt {
		if at > from && at <= to {
			n += int(c.secs[i])
		}
	}
	return n
}

// history returns per-bucket counts from bucket start (unix seconds,
// aligned) for n buckets.
func (c *pickCounter) history(start int64, n int) []int {
	out := make([]int, n)
	if c.hist == nil {
		return out
	}
	step := int64(accessHistBucket / time.Second)
	for k := 0; k < n; k++ {
		b := start/step + int64(k)
		j := int(b % int64(accessHistCap))
		if c.histAt[j] == b {
			out[k] = int(c.hist[j])
		}
	}
	return out
}

// accessRoute is one inbound tag's picks.
type accessRoute struct {
	outs map[string]*pickCounter
	// pool is the PoolKey last seen on this slot index by a query. Slot
	// indexes are reassigned when masters change; a different pool on
	// the same index starts from scratch.
	pool string
}

// xrayAccessLog is the per-inbound pick tracker plus the follower state of
// the log file it is fed from. Safe on a nil receiver (records nothing,
// answers empty).
type xrayAccessLog struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	routes map[string]*accessRoute

	following atomic.Bool // the file is open and being read
	note      atomic.Pointer[string]
	lines     atomic.Uint64 // accepted lines counted, for tests / debugging
}

func newXrayAccessLog(path string) *xrayAccessLog {
	a := &xrayAccessLog{path: path, now: time.Now, routes: map[string]*accessRoute{}}
	a.setNote("waiting for xray's access log")
	return a
}

// activeXrayAccess is the daemon's tracker, set by registerXrayActiveHandlers
// and fed by runXrayAccessLog. nil = not tracking.
var activeXrayAccess atomic.Pointer[xrayAccessLog]

func (a *xrayAccessLog) setNote(s string) { a.note.Store(&s) }

func (a *xrayAccessLog) status() (following bool, note string) {
	if a == nil {
		return false, "xray access log not tracked"
	}
	if p := a.note.Load(); p != nil {
		note = *p
	}
	return a.following.Load(), note
}

// parseAccessLine extracts the inbound and outbound tags of an accepted
// line ("... accepted DEST [IN -> OUT]..."). ok is false for anything else:
// rejected connections, lines without a detour, other log output. The
// returned slices alias line.
func parseAccessLine(line []byte) (in, out []byte, ok bool) {
	i := bytes.Index(line, []byte(" accepted "))
	if i < 0 {
		return nil, nil, false
	}
	rest := line[i+len(" accepted "):]
	sp := bytes.IndexByte(rest, ' ') // end of the destination
	if sp < 0 {
		return nil, nil, false
	}
	rest = rest[sp+1:]
	if len(rest) == 0 || rest[0] != '[' {
		return nil, nil, false
	}
	end := bytes.IndexByte(rest, ']')
	if end < 0 {
		return nil, nil, false
	}
	detour := rest[1:end]
	for _, sep := range [][]byte{[]byte(" -> "), []byte(" >> "), []byte(" ==> ")} {
		if k := bytes.Index(detour, sep); k >= 0 {
			in, out = detour[:k], detour[k+len(sep):]
			return in, out, len(in) > 0 && len(out) > 0
		}
	}
	// No inbound tag: xray logs the bare outbound. Nothing to attribute
	// it to.
	return nil, detour, false
}

// trackedInbound reports whether picks from inbound tag in are kept: a
// dialer slot's inbound, or an entry's. The API, metrics and path-probe
// inbounds are the daemon's own traffic.
func trackedInbound(in []byte) (slot bool, ok bool) {
	if bytes.HasPrefix(in, []byte("slot")) && bytes.HasSuffix(in, []byte("-in")) {
		return true, true
	}
	return false, bytes.HasPrefix(in, []byte("in-"))
}

// observe counts one log line read at t. Returns whether it was a pick.
func (a *xrayAccessLog) observe(line []byte, t time.Time) bool {
	if a == nil {
		return false
	}
	in, out, ok := parseAccessLine(line)
	if !ok {
		return false
	}
	slot, ok := trackedInbound(in)
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.routes[string(in)]
	if r == nil {
		if len(a.routes) >= accessMaxRoutes {
			return false
		}
		r = &accessRoute{outs: map[string]*pickCounter{}}
		a.routes[string(in)] = r
	}
	c := r.outs[string(out)]
	if c == nil {
		if len(r.outs) >= accessMaxOutbounds {
			return false
		}
		c = &pickCounter{}
		r.outs[string(out)] = c
	}
	c.add(t, slot)
	a.lines.Add(1)
	return true
}

// prune drops counters idle past the history span, and routes left empty.
func (a *xrayAccessLog) prune(now time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for in, r := range a.routes {
		for out, c := range r.outs {
			if now.Sub(c.last) > accessHistSpan {
				delete(r.outs, out)
			}
		}
		if len(r.outs) == 0 {
			delete(a.routes, in)
		}
	}
}

// outPick is one outbound's picks as a query sees them.
type outPick struct {
	tag     string
	picks   int // in the window
	recent  int // in accessRecent
	last    time.Time
	history []int
}

// slotPicks returns the picks slot's balancer made among its current
// members over window, busiest (recent, then window, then latest) first.
// Picks of tags that are no longer members are left out. hist > 0 adds
// that many history buckets ending at now's bucket; start is the first
// bucket's unix second.
func (a *xrayAccessLog) slotPicks(slot xray.DialerSlot, window time.Duration, hist int) (picks []outPick, start int64) {
	if a == nil {
		return nil, 0
	}
	now := a.now()
	if hist > 0 {
		step := int64(accessHistBucket / time.Second)
		start = (now.Unix()/step - int64(hist-1)) * step
	}
	members := make(map[string]bool, len(slot.Members))
	for _, m := range slot.Members {
		members[xray.SlotMemberTag(slot.Index, m.Key)] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.routes[xray.SlotInboundTag(slot.Index)]
	if r == nil {
		return nil, start
	}
	if r.pool != slot.PoolKey {
		if r.pool != "" {
			// Another pool took this slot index: its picks aren't ours.
			r.outs = map[string]*pickCounter{}
		}
		r.pool = slot.PoolKey
	}
	for tag, c := range r.outs {
		if !members[tag] {
			continue
		}
		p := outPick{tag: tag, picks: c.count(now, window), recent: c.count(now, min(window, accessRecent)), last: c.last}
		if hist > 0 {
			p.history = c.history(start, hist)
		}
		if p.picks == 0 && !hasPick(p.history) {
			continue
		}
		picks = append(picks, p)
	}
	sortPicks(picks)
	return picks, start
}

// inboundPicks returns every tracked entry inbound ("in-NAME") with its
// picks over window, busiest first per inbound. Inbounds with no pick in
// the window are left out.
func (a *xrayAccessLog) inboundPicks(window time.Duration) map[string][]outPick {
	out := map[string][]outPick{}
	if a == nil {
		return out
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for in, r := range a.routes {
		if !strings.HasPrefix(in, "in-") {
			continue
		}
		var ps []outPick
		for tag, c := range r.outs {
			if n := c.count(now, window); n > 0 {
				ps = append(ps, outPick{tag: tag, picks: n, recent: c.count(now, min(window, accessRecent)), last: c.last})
			}
		}
		if len(ps) > 0 {
			sortPicks(ps)
			out[in] = ps
		}
	}
	return out
}

func hasPick(h []int) bool {
	for _, n := range h {
		if n > 0 {
			return true
		}
	}
	return false
}

func sortPicks(ps []outPick) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].recent != ps[j].recent {
			return ps[i].recent > ps[j].recent
		}
		if ps[i].picks != ps[j].picks {
			return ps[i].picks > ps[j].picks
		}
		if !ps[i].last.Equal(ps[j].last) {
			return ps[i].last.After(ps[j].last)
		}
		return ps[i].tag < ps[j].tag
	})
}

// dominant reports whether the first of ps (sorted) carries the recent
// window on its own.
func dominant(ps []outPick) bool {
	if len(ps) == 0 {
		return false
	}
	total := 0
	for _, p := range ps {
		total += p.recent
	}
	top := ps[0].recent
	return top >= accessDominanceMin && float64(top) >= accessDominance*float64(total)
}
