package main

import (
	"sync"
	"time"
)

// Sticky upstream selection.
//
// Ranking is consulted per connection, so on its own it lets one page load
// scatter across several exit IPs: a browser opens six connections to the
// same origin, a probe round lands in the middle, and half of them leave
// from a different country than the other half. Origins treat that as a
// hijacked session — captcha, 403, forced re-login, dropped websockets.
// Observed in the wild on this daemon: chatgpt.com served from three
// different exits inside one minute.
//
// So we remember which upstream a destination is already using and feed it
// back to the ranker as the incumbent. The incumbent only keeps its place
// while it stays healthy AND no alternative is meaningfully faster (see
// netprobe's hysteresis margins) — a dead or clearly worse upstream is
// still replaced at once. This is a preference, never a pin.
const (
	// stickyTTL is how long an unused binding is remembered. Long enough
	// to span a browsing session on one site, short enough that the
	// ranking isn't frozen to a choice made an hour ago.
	stickyTTL = 10 * time.Minute

	// stickySweepInterval bounds how often the map is pruned. Pruning is
	// piggybacked on writes, so this only caps the work, never schedules it.
	stickySweepInterval = time.Minute
)

type stickyEntry struct {
	name string
	at   time.Time
}

// stickyBindings maps a destination (hostname, or IP literal when a rule
// matched by address) to the upstream currently carrying it. Thread-safe;
// a nil *stickyBindings is usable and simply never sticks.
type stickyBindings struct {
	mu        sync.Mutex
	entries   map[string]stickyEntry
	lastSweep time.Time
	now       func() time.Time // swappable for tests
}

func newStickyBindings() *stickyBindings {
	return &stickyBindings{entries: make(map[string]stickyEntry), now: time.Now}
}

// Get returns the upstream last known to carry key, or "" if there is none
// (or it has gone stale).
func (s *stickyBindings) Get(key string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || s.now().Sub(e.at) > stickyTTL {
		return ""
	}
	return e.name
}

// Set records name as the upstream carrying key, refreshing its TTL.
func (s *stickyBindings) Set(key, name string) {
	if s == nil || key == "" || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.entries[key] = stickyEntry{name: name, at: now}
	if now.Sub(s.lastSweep) >= stickySweepInterval {
		s.lastSweep = now
		for k, e := range s.entries {
			if now.Sub(e.at) > stickyTTL {
				delete(s.entries, k)
			}
		}
	}
}

// Drop forgets key's binding, but only if it still names the upstream the
// caller just found wanting — a concurrent connection may already have
// moved it, and clobbering that would undo a good decision.
func (s *stickyBindings) Drop(key, name string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && e.name == name {
		delete(s.entries, key)
	}
}
