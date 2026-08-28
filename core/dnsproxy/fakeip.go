package dnsproxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// DefaultFakeIPv4CIDR is the range fake IPs are drawn from when none is
// configured. 198.18.0.0/15 is the RFC 2544 benchmarking block: it's never
// globally routed, so a synthetic handle from it can't collide with a real
// destination the user might legitimately reach off-proxy.
const DefaultFakeIPv4CIDR = "198.18.0.0/15"

// DefaultFakeIPLeaseTTL is how long a hostname keeps its fake IP. It is
// deliberately far longer than the DNS answer TTL: clients cache addresses
// well past the record TTL (Spotify pins its access-point addresses
// in-process, browsers hold sockets open), and handing a still-cached slot
// to a different hostname sends one service's traffic to another. A /15 is
// 131072 slots, so holding leases for a week costs nothing in practice.
const DefaultFakeIPLeaseTTL = 7 * 24 * time.Hour

// fakeIPPersistRefreshFraction controls how often a lease refresh is
// written back. The stored expiry is allowed to trail the in-memory one by
// up to leaseTTL/4 before we spend a write, so a hot hostname re-queried
// every 30s doesn't hit the DB every 30s.
const fakeIPPersistRefreshFraction = 4

// FakeIPLease is one persisted hostname → fake-IP binding.
type FakeIPLease struct {
	Host      string
	IP        string
	ExpiresAt time.Time
}

// FakeIPStore persists fake-IP leases so the hostname → IP mapping
// survives a daemon restart. Without it the pool starts empty and reissues
// the same low addresses to whatever queries first, silently redirecting
// any client that still has a pre-restart fake IP cached. nil disables
// persistence (the pool stays memory-only, as before).
type FakeIPStore interface {
	ListFakeIPLeases(ctx context.Context, now time.Time) ([]FakeIPLease, error)
	PutFakeIPLease(ctx context.Context, lease FakeIPLease) error
	DeleteFakeIPLease(ctx context.Context, host string) error
	DeleteExpiredFakeIPLeases(ctx context.Context, now time.Time) error
}

// fakeIPPool hands out stable per-hostname synthetic IPv4 addresses from a
// reserved CIDR. The mapping is sticky for the lease: the same hostname
// gets the same IP while its lease is live, so a client's cached A record
// and the kernel host route stay consistent, and a re-query just refreshes
// the lease instead of churning addresses. With a store attached the
// bindings are reloaded at startup, so stickiness holds across restarts
// too.
//
// The pool only tracks occupancy here; the IP → (hostname, proxyNames)
// mapping the netstack handler actually dials lives in proxy.Table, written
// via ProxyResolver.Record. Slots are reclaimed lazily: an expired lease is
// reused the next time the scan lands on it.
type fakeIPPool struct {
	mu     sync.Mutex
	base   uint32 // first address in the block, as a uint32
	size   uint32 // number of addresses in the block
	cursor uint32 // round-robin offset for the next allocation scan
	ttl    time.Duration

	byHost map[string]*fakeLease
	byIP   map[uint32]*fakeLease

	store  FakeIPStore // nil → memory-only
	logger *log.Logger
}

type fakeLease struct {
	ip     uint32
	host   string
	expiry time.Time
	// stored is the expiry currently written to the FakeIPStore, so a
	// refresh only costs a write once it has drifted far enough. Zero
	// means "never persisted".
	stored time.Time
}

func newFakeIPPool(cidr string, ttl time.Duration, store FakeIPStore, logger *log.Logger) (*fakeIPPool, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", cidr, err)
	}
	v4 := ipnet.IP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("fake-ip range %q is not IPv4", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	if size < 2 {
		return nil, fmt.Errorf("fake-ip range %q too small", cidr)
	}
	if ttl <= 0 {
		ttl = DefaultFakeIPLeaseTTL
	}
	if logger == nil {
		logger = log.Default()
	}
	return &fakeIPPool{
		base:   binary.BigEndian.Uint32(v4),
		size:   size,
		ttl:    ttl,
		byHost: make(map[string]*fakeLease),
		byIP:   make(map[uint32]*fakeLease),
		store:  store,
		logger: logger,
	}, nil
}

// restore repopulates the pool from the store. Leases whose IP falls
// outside the configured block are dropped — the range may have been
// reconfigured between runs, and a lease we can't allocate from is worse
// than none. Errors are logged, not fatal: a pool that failed to restore
// still works, it just loses stickiness across this one restart.
func (p *fakeIPPool) restore(ctx context.Context) {
	if p.store == nil {
		return
	}
	now := time.Now()
	leases, err := p.store.ListFakeIPLeases(ctx, now)
	if err != nil {
		p.logger.Printf("dnsproxy: fake-ip restore: %v", err)
		return
	}

	p.mu.Lock()
	var maxOff uint32
	var dropped int
	for _, l := range leases {
		v4 := net.ParseIP(l.IP).To4()
		if v4 == nil {
			dropped++
			continue
		}
		v := binary.BigEndian.Uint32(v4)
		if v < p.base {
			dropped++
			continue
		}
		off := v - p.base
		if off == 0 || off >= p.size {
			dropped++
			continue
		}
		if _, taken := p.byIP[v]; taken {
			dropped++ // duplicate IP in the table; first row wins
			continue
		}
		lease := &fakeLease{ip: v, host: l.Host, expiry: l.ExpiresAt, stored: l.ExpiresAt}
		p.byHost[l.Host] = lease
		p.byIP[v] = lease
		if off > maxOff {
			maxOff = off
		}
	}
	// Start new allocations past the highest restored slot so a fresh
	// hostname doesn't land next to leases we just brought back.
	p.cursor = (maxOff + 1) % p.size
	restored := len(p.byHost)
	p.mu.Unlock()

	if err := p.store.DeleteExpiredFakeIPLeases(ctx, now); err != nil {
		p.logger.Printf("dnsproxy: fake-ip sweep: %v", err)
	}
	p.logger.Printf("dnsproxy: fake-ip restored %d leases (%d dropped)", restored, dropped)
}

// Allocate returns the fake IP bound to host, minting a new lease if
// needed. ok is false only when every slot holds an unexpired lease for
// some other host (pool exhausted).
func (p *fakeIPPool) Allocate(host string) (net.IP, bool) {
	ip, evicted, persist, ok := p.allocate(host)
	if !ok {
		return nil, false
	}
	// Store writes happen outside the lock: SQLite is local but a DNS
	// query shouldn't serialize behind a disk write.
	if p.store != nil {
		ctx := context.Background()
		if evicted != "" {
			if err := p.store.DeleteFakeIPLease(ctx, evicted); err != nil {
				p.logger.Printf("dnsproxy: fake-ip evict %s: %v", evicted, err)
			}
		}
		if persist != nil {
			if err := p.store.PutFakeIPLease(ctx, *persist); err != nil {
				p.logger.Printf("dnsproxy: fake-ip persist %s: %v", host, err)
			}
		}
	}
	return ip, true
}

// allocate does the in-memory half under the lock and reports what the
// caller should write back: evicted is a hostname whose expired lease was
// reclaimed, persist is the lease to upsert (nil when the refresh was too
// small to be worth a write).
func (p *fakeIPPool) allocate(host string) (ip net.IP, evicted string, persist *FakeIPLease, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	if l, found := p.byHost[host]; found {
		l.expiry = now.Add(p.ttl)
		if l.stored.IsZero() || l.expiry.Sub(l.stored) > p.ttl/fakeIPPersistRefreshFraction {
			l.stored = l.expiry
			persist = &FakeIPLease{Host: host, IP: intToIP(l.ip).String(), ExpiresAt: l.expiry}
		}
		return intToIP(l.ip), "", persist, true
	}

	// Scan for a slot that's free or whose lease has expired. Bounded by the
	// block size; offset 0 (the network address) is skipped.
	for i := uint32(0); i < p.size; i++ {
		off := (p.cursor + i) % p.size
		if off == 0 {
			continue
		}
		addr := p.base + off
		if l, taken := p.byIP[addr]; taken {
			if l.expiry.After(now) {
				continue // still leased to another host
			}
			// Expired — reclaim it.
			delete(p.byHost, l.host)
			delete(p.byIP, addr)
			evicted = l.host
		}
		expiry := now.Add(p.ttl)
		l := &fakeLease{ip: addr, host: host, expiry: expiry, stored: expiry}
		p.byHost[host] = l
		p.byIP[addr] = l
		p.cursor = (off + 1) % p.size
		return intToIP(addr), evicted, &FakeIPLease{Host: host, IP: intToIP(addr).String(), ExpiresAt: expiry}, true
	}
	return nil, "", nil, false
}

// Release frees the lease held by host, if any. Called when the work the
// allocation was for (route install) fails, so the slot is reusable
// immediately instead of lingering until its lease expires.
func (p *fakeIPPool) Release(host string) {
	p.mu.Lock()
	l, found := p.byHost[host]
	if found {
		delete(p.byHost, host)
		delete(p.byIP, l.ip)
	}
	p.mu.Unlock()

	if found && p.store != nil {
		if err := p.store.DeleteFakeIPLease(context.Background(), host); err != nil {
			p.logger.Printf("dnsproxy: fake-ip release %s: %v", host, err)
		}
	}
}

func intToIP(v uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b)
}
