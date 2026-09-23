package main

import (
	"fmt"
	"sync"
	"time"
)

// TCP destination health tracking — the connection-storm brake.
//
// The failure this exists for, observed in the wild on this daemon: 6308
// connections to code.claude.com:443 through one exit inside two minutes,
// and 8164 to anthropic.com inside another two. Normal browsing peaks under
// a hundred a minute. Nothing was looping internally — the client was
// retrying, and nothing on our side made retrying cost anything.
//
// The shape is the same one udpHealth documents, one layer up. An xray
// entry is a loopback SOCKS5 inbound that answers CONNECT before it has
// dialed the far side, so a dead exit accepts the connection, swallows the
// request and never answers. The client gives up and immediately opens
// another. Each of those costs us a netstack endpoint, a goroutine, and —
// because dialBinding walks the whole binding proxyDialMaxRounds times —
// up to fifteen upstream dials held for as long as proxyConnectDeadline.
// At a hundred client connections a second that is thousands of live
// goroutines blocked in syscalls, in this process and in xray's: two
// pegged cores and a four-figure thread count, which is exactly what a
// user reported.
//
// So we remember a destination that keeps failing and refuse it cheaply
// for a while instead of paying full price per retry. Refusing is not a
// downgrade in service: the destination is already unreachable through
// this binding, and closing at once beats stalling the client for twenty
// seconds first — the same reasoning udpHealth applies when it chooses
// ICMP over silence.
//
// Three things keep a live site from being tarpitted by mistake:
//
//   - A strike needs a total failure. Either every member of the binding
//     failed to dial, or the connection carried bytes out and got nothing
//     back. A partial failure that some member covered is not a strike.
//   - tcpStrikeThreshold consecutive strikes are needed, and a single byte
//     received clears the record outright.
//   - A penalised destination is never fully closed. One connection per
//     tcpProbeInterval is let through to test the path, so recovery is
//     noticed within a second rather than at the end of the penalty.
//
// The worst case a false positive can cost is therefore: after five
// connections in a row genuinely failed, some of the next second's
// connections are refused quickly instead of slowly, while one per second
// keeps checking. The storm case it prevents is five hundred connections a
// second, each holding fifteen upstream dials.
const (
	// tcpStrikeThreshold is how many CONSECUTIVE dead connections mark a
	// destination. Higher than udpStrikeThreshold on purpose: a client
	// aborting a TCP connection before the server answers is ordinary and
	// looks identical to a black hole from here, whereas breaking TCP for
	// a destination is far more visible than pushing it off QUIC.
	tcpStrikeThreshold = 5

	// tcpProbeInterval is how often ONE connection is admitted to a
	// penalised destination to see whether the path came back.
	tcpProbeInterval = time.Second

	// tcpHealthEntryTTL prunes destinations nothing has touched.
	tcpHealthEntryTTL = 2 * time.Hour
)

// tcpPenaltyLadder is how long a destination stays penalised after each
// successive marking. Much shorter than udpPenaltyLadder — there is no
// fallback transport below TCP, so every second of penalty is a second the
// user may be waiting on. Even the first rung is enough: it converts a
// hundred-per-second storm into five probes.
var tcpPenaltyLadder = []time.Duration{
	5 * time.Second,
	20 * time.Second,
	60 * time.Second,
}

type tcpHealthEntry struct {
	strikes   int
	level     int       // index into tcpPenaltyLadder for the NEXT penalty
	until     time.Time // penalised while now < until
	lastProbe time.Time // last connection admitted while penalised
	seen      time.Time
}

// tcpHealth remembers which destinations have a dead TCP path through
// their binding. Keyed by hostname (not IP), for the reason udpHealth
// gives: fake IPs are recycled across names, so an IP key would follow the
// lease rather than the destination.
type tcpHealth struct {
	mu      sync.Mutex
	entries map[string]*tcpHealthEntry
	now     func() time.Time // swappable for tests
}

func newTCPHealth() *tcpHealth {
	return &tcpHealth{entries: make(map[string]*tcpHealthEntry), now: time.Now}
}

func tcpHealthKey(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// admit reports whether this connection should be carried. A destination
// under penalty admits one connection per tcpProbeInterval — enough to
// notice recovery immediately, few enough that a retry storm costs
// nothing. Admitting a probe is recorded here, so concurrent callers can't
// all decide they are the probe.
func (h *tcpHealth) admit(key string) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[key]
	if e == nil {
		return true
	}
	now := h.now()
	e.seen = now
	if !now.Before(e.until) {
		return true
	}
	if now.Sub(e.lastProbe) >= tcpProbeInterval {
		e.lastProbe = now
		return true
	}
	return false
}

// strike records a connection that proved the path dead: every member of
// the binding failed to dial, or bytes went out and none came back. It
// returns the penalty just applied, or 0 if this strike didn't reach the
// threshold — the caller logs only on the transition, so a destination
// being hammered doesn't flood the log it is meant to protect.
func (h *tcpHealth) strike(key string) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.pruneLocked(now)

	e := h.entries[key]
	if e == nil {
		e = &tcpHealthEntry{}
		h.entries[key] = e
	}
	e.seen = now
	// A strike landing during an active penalty is the probe reporting
	// back: extend on the same rung rather than counting toward the next
	// one, so a destination that stays down doesn't escalate at probe rate.
	if now.Before(e.until) {
		e.until = now.Add(tcpPenaltyLadder[e.level])
		return 0
	}
	e.strikes++
	if e.strikes < tcpStrikeThreshold {
		return 0
	}
	e.strikes = 0
	d := tcpPenaltyLadder[e.level]
	if e.level < len(tcpPenaltyLadder)-1 {
		e.level++
	}
	e.until = now.Add(d)
	return d
}

// success clears a destination's record. One byte back is proof the path
// works, and a path that works is not one we have anything to remember
// about — including the escalation level, so a site that broke this
// morning doesn't start at a minute's penalty tonight.
// condemn applies the next penalty immediately, without waiting for
// tcpStrikeThreshold strikes — for failures that are a verdict on the
// destination rather than a hint (see errDestinationRefused). Repeats climb
// the same ladder as strikes do, and one byte received still clears it.
func (h *tcpHealth) condemn(key string) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.pruneLocked(now)
	e := h.entries[key]
	if e == nil {
		e = &tcpHealthEntry{}
		h.entries[key] = e
	}
	e.seen = now
	e.strikes = 0
	d := tcpPenaltyLadder[e.level]
	if e.level < len(tcpPenaltyLadder)-1 {
		e.level++
	}
	e.until = now.Add(d)
	return d
}

// reset forgets every destination's record. After a network change the
// old verdicts describe the old network.
func (h *tcpHealth) reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.entries = make(map[string]*tcpHealthEntry)
	h.mu.Unlock()
}

func (h *tcpHealth) success(key string) {
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

// pruneLocked drops entries nothing has touched for tcpHealthEntryTTL.
// Called from strike (the only unbounded-growth path); an entry under an
// active penalty is kept regardless of age.
func (h *tcpHealth) pruneLocked(now time.Time) {
	for k, e := range h.entries {
		if now.Before(e.until) {
			continue
		}
		if now.Sub(e.seen) > tcpHealthEntryTTL {
			delete(h.entries, k)
		}
	}
}
