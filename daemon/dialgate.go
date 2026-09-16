package main

import (
	"context"
	"time"
)

// Concurrent-dial ceiling.
//
// tcpHealth brakes a storm aimed at one destination. This is the backstop
// for the case it can't see: many destinations failing at once, which is
// what a single dead exit carrying a whole rule actually looks like. Each
// connection in flight through dialBinding costs a goroutine blocked in a
// syscall here, a SOCKS connection into xray, and an outbound dial in that
// process — so the number of them is the number that decides whether this
// daemon uses twenty threads or thirteen hundred.
//
// The ceiling covers the DIAL only. A spliced connection releases its slot
// before SpliceCounted runs, because a long-lived stream — a download, a
// websocket, an SSE response from an API — holds no dial resources and
// must never be able to starve new connections. Getting this backwards
// would be worse than having no ceiling at all: a handful of idle
// websockets would wedge the whole tunnel.
//
// Saturation queues rather than rejects. A browser opening sixty
// connections for one page must not have some of them refused because the
// other forty were still handshaking; it must have them served a moment
// later. Only a wait longer than a dial would itself have taken gives up,
// and by then the daemon is saturated by any measure.
const (
	// proxyMaxConcurrentDials is the ceiling. Well above what real
	// browsing reaches (tens), well below where thread count becomes a
	// problem (thousands).
	proxyMaxConcurrentDials = 256

	// proxyDialGateWait is how long a connection waits for a slot before
	// giving up. Kept under proxyDialAttemptTimeout so queueing can never
	// add more delay than the dial it is queueing for.
	proxyDialGateWait = 3 * time.Second
)

// dialGate bounds how many dials may be in flight at once. A nil *dialGate
// is usable and simply never blocks.
type dialGate struct {
	slots chan struct{}
}

func newDialGate(n int) *dialGate {
	return &dialGate{slots: make(chan struct{}, n)}
}

// acquire takes a slot, waiting up to proxyDialGateWait. It returns the
// release func and true on success; on failure the release func is still
// safe to call, so callers may defer it unconditionally. Release is
// idempotent for the same reason — a caller that releases early and then
// hits a deferred release must not return a slot twice.
func (g *dialGate) acquire(ctx context.Context) (func(), bool) {
	if g == nil {
		return func() {}, true
	}
	timer := time.NewTimer(proxyDialGateWait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		var released bool
		return func() {
			if released {
				return
			}
			released = true
			<-g.slots
		}, true
	case <-ctx.Done():
		return func() {}, false
	case <-timer.C:
		return func() {}, false
	}
}
