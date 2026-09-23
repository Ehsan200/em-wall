package main

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Resetting proxy health on a network change.
//
// Everything the proxy side has learned — paused destinations, demoted
// upstreams, measured latencies — describes the network it was learned on.
// After a Wi-Fi switch or a wake from sleep those verdicts are stale, and
// the first minute is spent obeying them. But the network watcher also
// fires on DNS-configuration churn (our own hijack and VPN apps rewrite
// it), so it can't trigger a reset by itself: a reset on every DNS rewrite
// would keep throwing away what was just learned. The reset therefore
// fires only on evidence the network really changed:
//
//   - the primary IPv4 route changed (interface, router or service), or
//   - the machine slept: the wall clock jumped past the monotonic clock,
//     which on macOS stops while asleep.

const (
	sleepCheckInterval = 10 * time.Second
	sleepGapThreshold  = 30 * time.Second
)

// primaryRoute fingerprints the primary IPv4 route from
// State:/Network/Global/IPv4. "" when it can't be read (no network).
func primaryRoute() string {
	cmd := exec.Command("/usr/sbin/scutil")
	cmd.Stdin = strings.NewReader("show State:/Network/Global/IPv4\n")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parsePrimaryRoute(string(out))
}

func parsePrimaryRoute(out string) string {
	var iface, router, service string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " : ")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "PrimaryInterface":
			iface = strings.TrimSpace(v)
		case "Router":
			router = strings.TrimSpace(v)
		case "PrimaryService":
			service = strings.TrimSpace(v)
		}
	}
	if iface == "" {
		return ""
	}
	return iface + "|" + router + "|" + service
}

// netResetter decides when a network event warrants a proxy-health reset.
type netResetter struct {
	mu    sync.Mutex
	route string
	read  func() string // primaryRoute; swappable for tests
	reset func(reason string)
}

func newNetResetter(reset func(reason string)) *netResetter {
	r := &netResetter{read: primaryRoute, reset: reset}
	r.route = r.read()
	return r
}

// networkEvent is called from the network watcher: reset only if the
// primary route actually moved.
func (r *netResetter) networkEvent() {
	now := r.read()
	r.mu.Lock()
	changed := now != r.route
	r.route = now
	r.mu.Unlock()
	if changed {
		r.reset("primary route changed")
	}
}

// watchSleep resets after the machine wakes. Blocks until ctx is done.
func (r *netResetter) watchSleep(ctx context.Context) {
	t := time.NewTicker(sleepCheckInterval)
	defer t.Stop()
	prev := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if slept(prev, now) {
				r.mu.Lock()
				r.route = r.read()
				r.mu.Unlock()
				r.reset("woke from sleep")
			}
			prev = now
		}
	}
}

// slept reports whether the wall clock advanced well past the monotonic
// clock between two readings — i.e. the machine was asleep in between.
func slept(prev, now time.Time) bool {
	// Round(0) strips the monotonic reading, leaving wall-clock time.
	return sleptGap(now.Round(0).Sub(prev.Round(0)), now.Sub(prev))
}

func sleptGap(wall, mono time.Duration) bool { return wall-mono > sleepGapThreshold }
