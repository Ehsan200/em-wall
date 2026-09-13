package main

import (
	"fmt"
	"sync"
	"time"
)

// UDP relay health tracking.
//
// A SOCKS5 UDP ASSOCIATE to an xray inbound succeeds whenever the inbound
// is up — it says nothing about whether the chosen outbound transport can
// actually carry datagrams end to end. When it can't, the association comes
// up, swallows everything the client sends, and never answers. For QUIC
// (UDP/443, which is how Chrome talks to every Google property) that reads
// as a browser sitting on a handshake until its own fallback timer fires:
// "Gmail is slow", "Docs won't load".
//
// We can't predict a black hole, but we can remember one. A flow that ends
// with bytes sent and nothing received is a strike against that destination.
// Two consecutive strikes and we stop accepting UDP for it for a while —
// and crucially we REJECT rather than swallow, so the stack answers ICMP
// port-unreachable and the client drops to TCP on its first packet instead
// of after a 20s stall. That is exactly the heuristic Chrome applies to its
// own "QUIC is broken for this origin" memory.
//
// Any successful flow clears the record, so a destination is never stuck on
// TCP once the path recovers.
const (
	// udpStrikeThreshold is how many CONSECUTIVE dead flows mark a
	// destination. One is too twitchy — a single association can come up
	// dead on an otherwise healthy proxy, and that recovers by itself on
	// the client's retry. Two in a row is a path problem.
	udpStrikeThreshold = 2

	// udpHealthEntryTTL prunes destinations nothing has touched. Keeps the
	// map proportional to what the user actually browses.
	udpHealthEntryTTL = 2 * time.Hour
)

// udpPenaltyLadder is how long UDP stays refused after each successive
// marking. Escalating: a destination whose QUIC keeps dying shouldn't be
// re-probed every five minutes forever, but the first strike must expire
// quickly so a transient outage doesn't pin a host on TCP for an hour.
var udpPenaltyLadder = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

type udpHealthEntry struct {
	strikes int
	level   int       // index into udpPenaltyLadder for the NEXT penalty
	until   time.Time // UDP refused while now < until
	seen    time.Time
}

// udpHealth remembers which destinations have a dead UDP relay path.
// Keyed by hostname (not IP): fake IPs are recycled across names, so an
// IP key would follow the lease rather than the destination.
type udpHealth struct {
	mu      sync.Mutex
	entries map[string]*udpHealthEntry
	now     func() time.Time // swappable for tests
}

func newUDPHealth() *udpHealth {
	return &udpHealth{entries: make(map[string]*udpHealthEntry), now: time.Now}
}

func udpHealthKey(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// blocked reports whether UDP to this destination is currently refused.
func (h *udpHealth) blocked(key string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[key]
	if e == nil {
		return false
	}
	now := h.now()
	e.seen = now
	return now.Before(e.until)
}

// strike records a flow that sent bytes and received none. It returns the
// penalty just applied, or 0 if this strike didn't reach the threshold —
// the caller logs only on the transition, so a destination that keeps
// getting retried doesn't flood the log.
func (h *udpHealth) strike(key string) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.pruneLocked(now)

	e := h.entries[key]
	if e == nil {
		e = &udpHealthEntry{}
		h.entries[key] = e
	}
	e.seen = now
	e.strikes++
	if e.strikes < udpStrikeThreshold {
		return 0
	}
	e.strikes = 0
	d := udpPenaltyLadder[e.level]
	if e.level < len(udpPenaltyLadder)-1 {
		e.level++
	}
	e.until = now.Add(d)
	return d
}

// success clears a destination's record. A path that works is not a path
// we have anything to remember about.
func (h *udpHealth) success(key string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[key]; e != nil {
		e.strikes = 0
		e.level = 0
		e.until = time.Time{}
		e.seen = h.now()
	}
}

// pruneLocked drops entries nothing has touched for udpHealthEntryTTL.
// Called from strike (the only unbounded-growth path); an entry under an
// active penalty is kept regardless of age.
func (h *udpHealth) pruneLocked(now time.Time) {
	for k, e := range h.entries {
		if now.Before(e.until) {
			continue
		}
		if now.Sub(e.seen) > udpHealthEntryTTL {
			delete(h.entries, k)
		}
	}
}
