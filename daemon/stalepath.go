package main

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

// Stale-path cleanup.
//
// A live apply (xray_live.go) changes the path new connections take, but
// xray never touches a connection that is already open: a stream dispatched
// through a master's old dialer member keeps riding it. When that member is
// the reason for the change (parked as dead, dropped by a subscription
// refresh, a dialer edit away from a broken pool), the stream just hangs —
// connIdle is 1800s, so Telegram shows "connecting…" for up to half an hour.
// Nothing can move an open stream to the new path (its far end lives on the
// remote server); only the client can, by reconnecting, and it does that
// only once its connection closes.
//
// So after a live apply every open connection through an affected entry is
// marked stale, and a stale one that then stalls — the client sent bytes
// and nothing came back for stalePathStallTimeout — is closed so the client
// reconnects onto the new path. A stale connection that keeps answering is
// never touched: the old path may well still work (the change may have been
// an unrelated member elsewhere in the pool), and a master's exit is its own
// server whichever member carries it, so moving a healthy stream gains
// nothing and costs the client a reconnect.
//
// There is no "kill by port" here as on Linux (ss -K on the slot port):
// the daemon holds both ends of every proxied connection, so it closes them
// itself, per entry rather than per shared slot, and macOS has no
// SOCK_DESTROY anyway. A restart needs none of this — it drops everything.

// stalePathStallTimeout is how long a stale connection may wait for a reply
// to bytes it sent before it is closed. Past a slow exit's first byte
// (proxyFirstByteTimeout), well inside the half-hour an app would otherwise
// sit on a dead stream. A long-poll caught here reconnects once, cheaply.
var stalePathStallTimeout = 8 * time.Second

// stalePathTick is how often the sweeper looks at stale connections.
var stalePathTick = time.Second

// liveConns is the registry of spliced TCP connections, by the upstream
// (proxy-store name) carrying each. The xray supervisor marks entries whose
// path changed; the sweeper closes the stale connections that stall.
//
// The world map (map.go) reads the same registry: each connection also
// carries what it is for (flow) and byte counters, sampled into a small
// rate window — only while the map is being watched.
type liveConns struct {
	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	stale  atomic.Int64 // stale connections still open; 0 = sweeper idles
	logger *log.Logger
	// watchUntil (unix nanos): the map polled recently, so the sweeper
	// tick samples rates. Past it, the tick does no per-connection work.
	watchUntil atomic.Int64
}

func newLiveConns(logger *log.Logger) *liveConns {
	return &liveConns{conns: map[*trackedConn]struct{}{}, logger: logger}
}

// trackedConn wraps a connection's upstream side to see when the client's
// bytes go unanswered. Writes are the client's bytes going out, reads are
// the replies.
type trackedConn struct {
	net.Conn
	name   string       // proxy-store name of the upstream
	client net.Conn     // closed together with the upstream
	flow   connFlow     // what the connection is for; immutable
	opened int64        // unix nanos at track
	since  atomic.Int64 // unix nanos of the oldest unanswered write; 0 = none
	stale  atomic.Bool
	closed atomic.Bool
	// up/down count bytes written (client → upstream) and read back. The
	// only per-packet cost the map adds: one atomic add per Read/Write.
	up, down atomic.Int64
	// rate is the sampled window, touched only under liveConns.mu.
	rate rateWindow
}

// connFlow is what a tracked connection carries, for the world map.
type connFlow struct {
	Host   string     // destination hostname ("" or the IP for IP rules)
	DestIP netip.Addr // local destination: a FakeIP for proxied names
	Port   uint16
	RuleID int64
	Sent   int64 // opening bytes already forwarded before the splice
}

func (t *trackedConn) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	if n > 0 {
		t.up.Add(int64(n))
		if t.since.Load() == 0 {
			t.since.CompareAndSwap(0, time.Now().UnixNano())
		}
	}
	return n, err
}

func (t *trackedConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.down.Add(int64(n))
		t.since.Store(0)
	}
	return n, err
}

// CloseWrite keeps the splice's half-close working through the wrapper.
func (t *trackedConn) CloseWrite() error {
	if cw, ok := t.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return t.Conn.SetWriteDeadline(time.Now())
}

// stalledFor reports how long the oldest unanswered write has waited.
func (t *trackedConn) stalledFor(now time.Time) time.Duration {
	s := t.since.Load()
	if s == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, s))
}

// track registers upstream (carried by name) for client's connection and
// returns the wrapper to splice through plus the func that unregisters it.
// Nil-safe: a nil registry returns upstream unchanged.
func (l *liveConns) track(name string, client, upstream net.Conn) (net.Conn, func()) {
	return l.trackFlow(name, connFlow{}, client, upstream)
}

// trackFlow is track with the connection's flow recorded for the map.
func (l *liveConns) trackFlow(name string, flow connFlow, client, upstream net.Conn) (net.Conn, func()) {
	if l == nil {
		return upstream, func() {}
	}
	t := &trackedConn{Conn: upstream, name: name, client: client, flow: flow, opened: time.Now().UnixNano()}
	t.up.Store(flow.Sent)
	l.mu.Lock()
	l.conns[t] = struct{}{}
	l.mu.Unlock()
	return t, func() {
		l.mu.Lock()
		delete(l.conns, t)
		l.mu.Unlock()
		if t.stale.Load() {
			l.stale.Add(-1)
		}
	}
}

// markStale flags every open connection through one of names. Returns how
// many were newly marked.
func (l *liveConns) markStale(names map[string]bool) int {
	if l == nil || len(names) == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for t := range l.conns {
		if names[t.name] && t.stale.CompareAndSwap(false, true) {
			l.stale.Add(1)
			n++
		}
	}
	return n
}

// sweep closes the stale connections that have stalled; returns the count.
func (l *liveConns) sweep(now time.Time) int {
	if l == nil || l.stale.Load() == 0 {
		return 0
	}
	var victims []*trackedConn
	l.mu.Lock()
	for t := range l.conns {
		if t.stale.Load() && t.stalledFor(now) >= stalePathStallTimeout && t.closed.CompareAndSwap(false, true) {
			victims = append(victims, t)
		}
	}
	l.mu.Unlock()
	for _, t := range victims {
		_ = t.client.Close()
		_ = t.Conn.Close()
	}
	return len(victims)
}

// run sweeps until ctx is done.
func (l *liveConns) run(ctx context.Context) {
	if l == nil {
		return
	}
	t := time.NewTicker(stalePathTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.sampleRates(now)
			if n := l.sweep(now); n > 0 && l.logger != nil {
				l.logger.Printf("proxytun: closed %d stalled connection(s) left on a changed xray path — clients reconnect onto the new one", n)
			}
		}
	}
}

// changedPathEntries names the xray entries (as hidden proxy-store names)
// whose outbound path a live apply changed, from the tags it removed or
// replaced: an entry's own outbound or inbound, a master's dialer outbound,
// or any member of a slot a master was on (every master sharing that slot
// is affected — the balancer may have put any of their streams on it).
// Added tags change nothing for open connections.
func changedPathEntries(p livePlan, oldSlots []xray.DialerSlot) map[string]bool {
	out := map[string]bool{}
	add := func(entry string) { out[xray.InternalProxyName(entry)] = true }
	slotHit := map[int]bool{}
	for _, tag := range append(append([]string(nil), p.rmOut...), p.rmIn...) {
		switch {
		case strings.HasPrefix(tag, "out-"):
			add(strings.TrimPrefix(tag, "out-"))
		case strings.HasPrefix(tag, "in-"):
			add(strings.TrimPrefix(tag, "in-"))
		case strings.HasPrefix(tag, "dialer-"):
			add(strings.TrimPrefix(tag, "dialer-"))
		case strings.HasPrefix(tag, "slot"):
			if i, ok := slotIndexOf(tag); ok {
				slotHit[i] = true
			}
		}
	}
	for _, s := range oldSlots {
		if slotHit[s.Index] {
			for _, m := range s.SlotMasters() {
				add(m)
			}
		}
	}
	return out
}

// slotIndexOf parses N from a slot member tag "slotN-out-…".
func slotIndexOf(tag string) (int, bool) {
	rest := strings.TrimPrefix(tag, "slot")
	num, suffix, ok := strings.Cut(rest, "-")
	if !ok || !strings.HasPrefix(suffix, "out-") {
		return 0, false
	}
	i, err := strconv.Atoi(num)
	return i, err == nil
}

// Rate window for the world map.
//
// The splice path only adds to two atomic counters. Turning them into
// bytes/s is done off that path: while the map is being watched (it polls
// every couple of seconds), the sweeper's one-second tick copies each
// connection's counters into a small ring, and a rate is the difference
// across the ring. Unwatched, the tick skips it — a closed Map tab costs
// nothing per connection.

const (
	rateSlots = 5 // one-second samples: rates cover the last ~4s
	// mapWatchTTL keeps sampling on this long after the last map poll.
	mapWatchTTL = 15 * time.Second
)

type rateSample struct{ at, up, down int64 }

// rateWindow is a ring of counter samples, oldest overwritten first.
type rateWindow struct {
	s    [rateSlots]rateSample
	next int // where the next sample goes
	n    int // samples held, ≤ rateSlots
}

// rateGap: a sample this far past the newest one means sampling was off in
// between, and the ring restarts rather than average across the gap.
const rateGap = 3 * time.Second

func (w *rateWindow) add(s rateSample) {
	if w.n > 0 && s.at-w.s[(w.next-1+rateSlots)%rateSlots].at > int64(rateGap) {
		w.n = 0
	}
	w.s[w.next] = s
	w.next = (w.next + 1) % rateSlots
	if w.n < rateSlots {
		w.n++
	}
}

// rates returns bytes/s from the oldest held sample to cur; ok is false
// with fewer than one sample or no time elapsed.
func (w *rateWindow) rates(cur rateSample) (up, down float64, ok bool) {
	if w.n == 0 {
		return 0, 0, false
	}
	oldest := w.s[(w.next-w.n+rateSlots)%rateSlots]
	dt := float64(cur.at-oldest.at) / float64(time.Second)
	if dt <= 0 {
		return 0, 0, false
	}
	return float64(max(0, cur.up-oldest.up)) / dt, float64(max(0, cur.down-oldest.down)) / dt, true
}

// watch notes that the map is reading; sampling stays on for mapWatchTTL.
func (l *liveConns) watch(now time.Time) {
	if l != nil {
		l.watchUntil.Store(now.Add(mapWatchTTL).UnixNano())
	}
}

// sampleRates records every open connection's counters while watched.
func (l *liveConns) sampleRates(now time.Time) {
	if l == nil || now.UnixNano() > l.watchUntil.Load() {
		return
	}
	at := now.UnixNano()
	l.mu.Lock()
	for t := range l.conns {
		t.rate.add(rateSample{at: at, up: t.up.Load(), down: t.down.Load()})
	}
	l.mu.Unlock()
}

// liveFlow is one open connection as the map sees it.
type liveFlow struct {
	name             string // proxy-store name of the upstream
	flow             connFlow
	up, down         int64   // bytes since open
	rateUp, rateDown float64 // bytes/s over the rate window
}

// flows snapshots every open connection. A connection without samples yet
// (just opened, or sampling just switched on) is rated by its lifetime
// average, which for a young connection is close to the truth and for an
// old one is at least the right order of magnitude for one poll.
func (l *liveConns) flows(now time.Time) []liveFlow {
	if l == nil {
		return nil
	}
	at := now.UnixNano()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]liveFlow, 0, len(l.conns))
	for t := range l.conns {
		cur := rateSample{at: at, up: t.up.Load(), down: t.down.Load()}
		f := liveFlow{name: t.name, flow: t.flow, up: cur.up, down: cur.down}
		var ok bool
		if f.rateUp, f.rateDown, ok = t.rate.rates(cur); !ok {
			if age := float64(at-t.opened) / float64(time.Second); age > 0 {
				f.rateUp, f.rateDown = float64(cur.up)/age, float64(cur.down)/age
			}
		}
		out = append(out, f)
	}
	return out
}
