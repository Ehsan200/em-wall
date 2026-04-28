package applocator

import (
	"sync"
	"time"
)

// LsofProvider abstracts how we obtain the per-utun process map.
// In production it's wired to routing.LsofUtunOwners; in tests we
// inject a static map.
type LsofProvider interface {
	LsofUtunOwners() map[string]string
}

// AppChange is reported when an app's owning utun number changes
// (including coming up or going away).
type AppChange struct {
	Key string
	Old string // previous utun ("" if app wasn't running)
	New string // current utun ("" if app stopped)
}

// Resolver owns the live app→utun mapping and the per-app
// transition locks. Read-locks are cheap and concurrent; the watcher
// takes a write-lock per app while the route set is being updated,
// causing in-flight queries to wait briefly.
type Resolver struct {
	provider LsofProvider
	apps     []App

	mu      sync.RWMutex
	current map[string]string // app key → current utun

	locksMu sync.Mutex
	locks   map[string]*sync.RWMutex
}

func NewResolver(provider LsofProvider) *Resolver {
	return &Resolver{
		provider: provider,
		apps:     KnownApps(),
		current:  map[string]string{},
		locks:    map[string]*sync.RWMutex{},
	}
}

// Apps returns the app registry.
func (r *Resolver) Apps() []App { return r.apps }

// Current returns the utun owned by the named app, or "" if not running.
func (r *Resolver) Current(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current[key]
}

// FirstAvailable walks keys in order and returns the first one that
// currently has a running utun, along with that utun. Returns "", ""
// if none of them are running. Read-only — does not take any lock.
func (r *Resolver) FirstAvailable(keys []string) (key, iface string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range keys {
		if v := r.current[k]; v != "" {
			return k, v
		}
	}
	return "", ""
}

// Snapshot returns a copy of the current mapping.
func (r *Resolver) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.current))
	for k, v := range r.current {
		out[k] = v
	}
	return out
}

// Refresh polls the lsof provider, recomputes the app→utun mapping,
// and returns a list of changes since the last call. Caller is
// responsible for taking the write-lock around any state mutation
// based on the returned changes.
func (r *Resolver) Refresh() []AppChange {
	owners := r.provider.LsofUtunOwners()
	now := make(map[string]string, len(r.apps))
	for iface, proc := range owners {
		for _, app := range r.apps {
			if app.MatchesProcess(proc) {
				now[app.Key] = iface
				break
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var changes []AppChange
	for _, app := range r.apps {
		old := r.current[app.Key]
		nw := now[app.Key]
		if old != nw {
			changes = append(changes, AppChange{Key: app.Key, Old: old, New: nw})
		}
	}
	r.current = now
	return changes
}

func (r *Resolver) lockFor(key string) *sync.RWMutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	if l, ok := r.locks[key]; ok {
		return l
	}
	l := &sync.RWMutex{}
	r.locks[key] = l
	return l
}

// AcquireForRead blocks (up to timeout) waiting for a read-slot on
// the per-app lock. Returns a release function and ok=true when the
// caller holds the lock; ok=false means the caller timed out and
// MUST NOT proceed (release is a no-op).
//
// Read-locks are how DNS-query workers ensure they only see a stable
// mapping during a transition.
func (r *Resolver) AcquireForRead(key string, timeout time.Duration) (release func(), ok bool) {
	l := r.lockFor(key)
	deadline := time.Now().Add(timeout)
	const probe = 5 * time.Millisecond
	for {
		if l.TryRLock() {
			return l.RUnlock, true
		}
		if !time.Now().Before(deadline) {
			return func() {}, false
		}
		time.Sleep(probe)
	}
}

// AcquireForWrite takes the per-app write-lock. The watcher uses
// this around the (flush-old-routes → record-new-utun) sequence.
func (r *Resolver) AcquireForWrite(key string) (release func()) {
	l := r.lockFor(key)
	l.Lock()
	return l.Unlock
}
