package main

import (
	"context"
	"log"
	"net"
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
type liveConns struct {
	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	stale  atomic.Int64 // stale connections still open; 0 = sweeper idles
	logger *log.Logger
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
	since  atomic.Int64 // unix nanos of the oldest unanswered write; 0 = none
	stale  atomic.Bool
	closed atomic.Bool
}

func (t *trackedConn) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	if n > 0 {
		t.since.CompareAndSwap(0, time.Now().UnixNano())
	}
	return n, err
}

func (t *trackedConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
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
	if l == nil {
		return upstream, func() {}
	}
	t := &trackedConn{Conn: upstream, name: name, client: client}
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
