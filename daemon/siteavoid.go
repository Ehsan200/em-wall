package main

import (
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
)

// Per-site avoidance.
//
// Ranking is global: a member's cost says how well it carries traffic in
// general. But some failures are about one site only — an exit IP a
// service has banned (observed: every member of the nj set shared an exit
// Spotify refused), a node whose DNS can't resolve one CDN, a route that
// throttles one host. A failure already drops the site's sticky binding,
// yet the next connection ranks from scratch and, since the member is
// still healthy everywhere else, picks the very same member again. The
// site keeps failing on its best-ranked member while a sibling that works
// for it sits one position down.
//
// So a member that failed a site siteAvoidStrikes times within
// siteAvoidWindow is ranked behind the other members for that site for
// siteAvoidFor. It is never removed — when every member is avoided the
// ranking is used as is — and any sign it carries the site again (it
// answered a handshake for it, or data came back) clears the record.
//
// A strike is also recorded against the member's exit IP when it is known
// (exitKeys): a ban is on the address, so a sibling leaving through the
// same exit is avoided for that site too, without first failing on it.
const (
	siteAvoidStrikes = 2
	siteAvoidWindow  = 10 * time.Minute
	siteAvoidFor     = 15 * time.Minute

	// siteAvoidSweepInterval bounds how often expired records are pruned;
	// pruning piggybacks on writes.
	siteAvoidSweepInterval = time.Minute
)

type avoidRecord struct {
	strikes    int
	firstAt    time.Time // start of the current strike window
	avoidUntil time.Time // zero = not avoided
}

// siteAvoid tracks (site, member) pairs that should be tried last. Keys
// for a member are its proxy name and, when known, "exit:"+IP. Safe on a
// nil receiver, which avoids nothing.
type siteAvoid struct {
	mu        sync.Mutex
	recs      map[string]*avoidRecord // site + "|" + member key
	lastSweep time.Time
	now       func() time.Time
}

func newSiteAvoid() *siteAvoid {
	return &siteAvoid{recs: map[string]*avoidRecord{}, now: time.Now}
}

// avoidSite is the key avoidance is tracked under for a destination: its
// site when it has one (so every host of a CDN shares the verdict), else
// the host or IP itself.
func avoidSite(entry proxy.Entry) string {
	if s := siteKey(entry.Hostname); s != "" {
		return s
	}
	return stickyKey(entry)
}

// strike records one failure of each key for site and reports whether the
// first key (the member itself) just became avoided.
func (a *siteAvoid) strike(site string, keys ...string) bool {
	if a == nil || site == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.sweepLocked(now)
	became := false
	for i, k := range keys {
		if k == "" {
			continue
		}
		id := site + "|" + k
		r := a.recs[id]
		if r == nil || now.Sub(r.firstAt) > siteAvoidWindow {
			r = &avoidRecord{firstAt: now, avoidUntil: timeOr(r)}
			a.recs[id] = r
		}
		r.strikes++
		if r.strikes >= siteAvoidStrikes {
			wasAvoided := now.Before(r.avoidUntil)
			r.avoidUntil = now.Add(siteAvoidFor)
			r.strikes = 0
			r.firstAt = now
			if i == 0 && !wasAvoided {
				became = true
			}
		}
	}
	return became
}

// timeOr carries an active avoidance over into a fresh strike window.
func timeOr(r *avoidRecord) time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.avoidUntil
}

// clear forgets every record of keys for site: the member just carried it.
func (a *siteAvoid) clear(site string, keys ...string) {
	if a == nil || site == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range keys {
		if k != "" {
			delete(a.recs, site+"|"+k)
		}
	}
}

// avoidedLocked reports whether any of keys is avoided for site.
func (a *siteAvoid) avoidedLocked(site string, now time.Time, keys []string) bool {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if r := a.recs[site+"|"+k]; r != nil && now.Before(r.avoidUntil) {
			return true
		}
	}
	return false
}

// demote moves the names avoided for site behind the others, keeping the
// ranking's order within each group. keysOf gives a name's keys (name
// first). When every name is avoided the ranking is returned untouched:
// the least-bad member is still the ranking's first choice.
func (a *siteAvoid) demote(site string, ranked []string, keysOf func(string) []string) []string {
	if a == nil || site == "" || len(ranked) < 2 {
		return ranked
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.recs) == 0 {
		return ranked
	}
	now := a.now()
	var ok, avoided []string
	for _, n := range ranked {
		if a.avoidedLocked(site, now, keysOf(n)) {
			avoided = append(avoided, n)
		} else {
			ok = append(ok, n)
		}
	}
	if len(avoided) == 0 || len(ok) == 0 {
		return ranked
	}
	return append(ok, avoided...)
}

func (a *siteAvoid) sweepLocked(now time.Time) {
	if now.Sub(a.lastSweep) < siteAvoidSweepInterval {
		return
	}
	a.lastSweep = now
	for id, r := range a.recs {
		if now.Sub(r.firstAt) > siteAvoidWindow && !now.Before(r.avoidUntil) {
			delete(a.recs, id)
		}
	}
}

// reset forgets everything. For a real network change: a site that failed
// through a member on the old network says nothing about the new one.
func (a *siteAvoid) reset() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.recs = map[string]*avoidRecord{}
	a.mu.Unlock()
}
