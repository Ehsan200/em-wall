package proxy

import (
	"net"
	"sync"
	"time"
)

// Entry maps an IP address (resolved at DNS time) to the proxy
// reference the DNS rule chose for it, plus the hostname that was
// queried. The hostname matters: when the daemon dials the upstream
// proxy it should pass the original hostname (not the IP) so SNI and
// proxy-side hostname ACLs stay intact end-to-end.
//
// One IP can map to multiple ProxyNames — same multi-binding fallback
// semantics as app:KEY1,KEY2. The first reachable one wins at
// connection time.
type Entry struct {
	ProxyNames []string
	Hostname   string
	ExpiresAt  time.Time
	RuleID     int64
}

// Table is the IP → proxy mapping populated by the DNS layer and
// consumed by the netstack handler when a TCP connection arrives at
// our utun. Thread-safe; reads are common, writes happen once per
// DNS answer.
type Table struct {
	mu      sync.RWMutex
	byIP    map[string]Entry
	byRule  map[int64]map[string]struct{} // ruleID → set of IP strings
	minTTL  time.Duration
}

// NewTable returns an empty Table. minTTL is the minimum lifetime of
// a recorded mapping — DNS TTLs below this are bumped up so a
// transient TCP connection isn't orphaned by an aggressive sweep.
func NewTable(minTTL time.Duration) *Table {
	if minTTL <= 0 {
		minTTL = 60 * time.Second
	}
	return &Table{
		byIP:   map[string]Entry{},
		byRule: map[int64]map[string]struct{}{},
		minTTL: minTTL,
	}
}

// Record stores the mapping ip → (proxyNames, hostname). ttl is the
// DNS answer's TTL; the entry expires after max(ttl, minTTL). An
// existing entry for the same IP is overwritten (last write wins —
// the most recent DNS answer is authoritative).
func (t *Table) Record(ip net.IP, hostname string, proxyNames []string, ttl time.Duration, ruleID int64) {
	if ip == nil || len(proxyNames) == 0 {
		return
	}
	if ttl < t.minTTL {
		ttl = t.minTTL
	}
	key := ip.String()
	expires := time.Now().Add(ttl)

	// Copy proxyNames so caller mutations don't leak in.
	pn := make([]string, len(proxyNames))
	copy(pn, proxyNames)

	t.mu.Lock()
	defer t.mu.Unlock()

	if prev, ok := t.byIP[key]; ok && prev.RuleID != ruleID {
		// The IP was previously bound to a different rule. Detach it
		// from the old rule's index so RemoveByRule on that rule
		// doesn't try to delete this fresh entry.
		if set := t.byRule[prev.RuleID]; set != nil {
			delete(set, key)
			if len(set) == 0 {
				delete(t.byRule, prev.RuleID)
			}
		}
	}

	t.byIP[key] = Entry{
		ProxyNames: pn,
		Hostname:   hostname,
		ExpiresAt:  expires,
		RuleID:     ruleID,
	}
	if t.byRule[ruleID] == nil {
		t.byRule[ruleID] = map[string]struct{}{}
	}
	t.byRule[ruleID][key] = struct{}{}
}

// Lookup returns the Entry for ip, or (zero, false) if no mapping or
// it has expired. Expired entries are NOT removed by Lookup — that's
// the sweep loop's job; otherwise concurrent lookups would churn the
// write lock.
func (t *Table) Lookup(ip net.IP) (Entry, bool) {
	if ip == nil {
		return Entry{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.byIP[ip.String()]
	if !ok {
		return Entry{}, false
	}
	if time.Now().After(e.ExpiresAt) {
		return Entry{}, false
	}
	return e, true
}

// RemoveByRule deletes every mapping that was installed for ruleID.
// Used when a rule is deleted or updated so the table doesn't keep
// pointing stale IPs at a no-longer-meaningful proxy.
func (t *Table) RemoveByRule(ruleID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.byRule[ruleID]
	if set == nil {
		return 0
	}
	for ipStr := range set {
		delete(t.byIP, ipStr)
	}
	n := len(set)
	delete(t.byRule, ruleID)
	return n
}

// SweepExpired removes entries whose ExpiresAt has passed. Caller
// should invoke from a ticker; returns the count removed.
func (t *Table) SweepExpired() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var n int
	for ip, e := range t.byIP {
		if now.After(e.ExpiresAt) {
			delete(t.byIP, ip)
			if set := t.byRule[e.RuleID]; set != nil {
				delete(set, ip)
				if len(set) == 0 {
					delete(t.byRule, e.RuleID)
				}
			}
			n++
		}
	}
	return n
}

// Len returns the current number of non-expired entries (the
// underlying map size; cheaper than walking and filtering — callers
// observing this near a sweep should treat it as approximate).
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byIP)
}
