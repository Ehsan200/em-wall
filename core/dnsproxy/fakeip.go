package dnsproxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// DefaultFakeIPv4CIDR is the range fake IPs are drawn from when none is
// configured. 198.18.0.0/15 is the RFC 2544 benchmarking block: it's never
// globally routed, so a synthetic handle from it can't collide with a real
// destination the user might legitimately reach off-proxy.
const DefaultFakeIPv4CIDR = "198.18.0.0/15"

// fakeIPPool hands out stable per-hostname synthetic IPv4 addresses from a
// reserved CIDR. The mapping is intentionally sticky for a lease: the same
// hostname gets the same IP while its lease is live, so a client's cached A
// record and the kernel host route stay consistent, and a re-query just
// refreshes the lease instead of churning addresses.
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
}

type fakeLease struct {
	ip     uint32
	host   string
	expiry time.Time
}

func newFakeIPPool(cidr string, ttl time.Duration) (*fakeIPPool, error) {
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
		ttl = 30 * time.Second
	}
	return &fakeIPPool{
		base:   binary.BigEndian.Uint32(v4),
		size:   size,
		ttl:    ttl,
		byHost: make(map[string]*fakeLease),
		byIP:   make(map[uint32]*fakeLease),
	}, nil
}

// Allocate returns the fake IP bound to host, minting a new lease if needed.
// The second return is the lease TTL (used for the host route + A record);
// ok is false only when every slot holds an unexpired lease for some other
// host (pool exhausted).
func (p *fakeIPPool) Allocate(host string) (net.IP, time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	if l, ok := p.byHost[host]; ok {
		l.expiry = now.Add(p.ttl)
		return intToIP(l.ip), p.ttl, true
	}

	// Scan for a slot that's free or whose lease has expired. Bounded by the
	// block size; offset 0 (the network address) is skipped.
	for i := uint32(0); i < p.size; i++ {
		off := (p.cursor + i) % p.size
		if off == 0 {
			continue
		}
		ip := p.base + off
		if l, taken := p.byIP[ip]; taken {
			if l.expiry.After(now) {
				continue // still leased to another host
			}
			// Expired — reclaim it.
			delete(p.byHost, l.host)
			delete(p.byIP, ip)
		}
		l := &fakeLease{ip: ip, host: host, expiry: now.Add(p.ttl)}
		p.byHost[host] = l
		p.byIP[ip] = l
		p.cursor = (off + 1) % p.size
		return intToIP(ip), p.ttl, true
	}
	return nil, 0, false
}

// Release frees the lease held by host, if any. Called when the work the
// allocation was for (route install) fails, so the slot is reusable
// immediately instead of lingering until its lease expires.
func (p *fakeIPPool) Release(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l, ok := p.byHost[host]; ok {
		delete(p.byHost, host)
		delete(p.byIP, l.ip)
	}
}

func intToIP(v uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b)
}
