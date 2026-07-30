package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/rules"
)

// trafficDrainInterval is how often the in-memory coalescer flushes
// accumulated byte deltas to the store. Proxy flush callbacks fire often
// (one per connection every ~20s, plus on close); coalescing into a map
// and draining on a slower tick keeps SQLite writes batched.
const trafficDrainInterval = 10 * time.Second

// trafficKey is the composite bucket identity. The bucket timestamp is
// captured at Record time so a delta is attributed to the 5-minute window
// it actually flowed in, even if the drain runs in a later window.
type trafficKey struct {
	bucketTS int64
	domain   string
	proxy    string
}

type trafficSum struct {
	sent int64
	recv int64
}

// trafficAggregator buffers proxied byte deltas in memory and drains them
// into the store on a ticker. Record is called from the proxy splice flush
// callbacks (hot path, must be cheap + non-blocking); the actual DB writes
// and group attribution happen in the drain goroutine.
type trafficAggregator struct {
	store  *rules.Store
	logger *log.Logger

	mu  sync.Mutex
	buf map[trafficKey]*trafficSum
}

func newTrafficAggregator(store *rules.Store, logger *log.Logger) *trafficAggregator {
	return &trafficAggregator{
		store:  store,
		logger: logger,
		buf:    make(map[trafficKey]*trafficSum),
	}
}

// Record adds a sent/recv byte delta for (domain, proxy), bucketed by the
// current time. Safe for concurrent callers. A blank domain is stored as
// "(unknown)" so it's still visible rather than silently dropped.
func (a *trafficAggregator) Record(domain, proxy string, sent, recv int64) {
	if sent == 0 && recv == 0 {
		return
	}
	if domain == "" {
		domain = "(unknown)"
	}
	k := trafficKey{
		bucketTS: rules.FloorBucket(time.Now()),
		domain:   domain,
		proxy:    proxy,
	}
	a.mu.Lock()
	s := a.buf[k]
	if s == nil {
		s = &trafficSum{}
		a.buf[k] = s
	}
	s.sent += sent
	s.recv += recv
	a.mu.Unlock()
}

// drain swaps out the buffer and writes each entry to the store, resolving
// the key→group attribution at write time. The key is a hostname for
// DNS-routed flows or a destination IP for IP/CIDR-routed flows; both are
// attributed via groupForKey so IP traffic (e.g. Telegram MTProto DCs) and
// user-defined custom groups land under their group rather than "ungrouped".
func (a *trafficAggregator) drain(ctx context.Context) {
	a.mu.Lock()
	if len(a.buf) == 0 {
		a.mu.Unlock()
		return
	}
	pending := a.buf
	a.buf = make(map[trafficKey]*trafficSum)
	a.mu.Unlock()

	// Custom groups live in the DB (unlike the code-defined curated groups);
	// fetch them once per drain and reuse across every pending key.
	custom, err := a.store.ListCustomGroups(ctx)
	if err != nil {
		a.logger.Printf("em-walld: traffic group attribution: list custom groups failed: %v", err)
		custom = nil
	}

	for k, s := range pending {
		groupKey := a.groupForKey(k.domain, custom)
		if err := a.store.AddTraffic(ctx, k.bucketTS, k.domain, k.proxy, groupKey, s.sent, s.recv); err != nil {
			a.logger.Printf("em-walld: traffic stat write failed: %v", err)
		}
	}
}

// groupForKey resolves key (hostname or IP string) to a group key: curated
// groups first (so a built-in product group wins), then user custom groups.
// Returns "" (ungrouped) when nothing claims it.
func (a *trafficAggregator) groupForKey(key string, custom []rules.CustomGroup) string {
	if gkey, _, ok := groups.GroupForKey(key); ok {
		return gkey
	}
	for _, g := range custom {
		for _, pat := range g.Patterns {
			if rules.MatchKey(pat, key) {
				return g.Key
			}
		}
	}
	return ""
}

// Run drives the periodic drain until ctx is cancelled, then flushes once
// more so in-flight buffered bytes aren't lost on shutdown.
func (a *trafficAggregator) Run(ctx context.Context) {
	t := time.NewTicker(trafficDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.drain(context.Background())
			return
		case <-t.C:
			a.drain(ctx)
		}
	}
}
