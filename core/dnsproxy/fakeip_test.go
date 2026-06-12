package dnsproxy

import (
	"net"
	"testing"
	"time"
)

func mustPool(t *testing.T, cidr string, ttl time.Duration) *fakeIPPool {
	t.Helper()
	p, err := newFakeIPPool(cidr, ttl)
	if err != nil {
		t.Fatalf("newFakeIPPool(%q): %v", cidr, err)
	}
	return p
}

// A hostname always maps to the same IP while its lease is live, and the IP
// comes from the configured block.
func TestFakeIPPool_StablePerHost(t *testing.T) {
	p := mustPool(t, "198.18.0.0/15", 30*time.Second)
	_, block, _ := net.ParseCIDR("198.18.0.0/15")

	ip1, ttl, ok := p.Allocate("a.googlevideo.com")
	if !ok {
		t.Fatal("allocate failed")
	}
	if ttl != 30*time.Second {
		t.Fatalf("ttl = %v, want 30s", ttl)
	}
	if !block.Contains(ip1) {
		t.Fatalf("ip %s not in 198.18.0.0/15", ip1)
	}
	ip2, _, _ := p.Allocate("a.googlevideo.com")
	if !ip1.Equal(ip2) {
		t.Fatalf("same host got different IPs: %s vs %s", ip1, ip2)
	}
}

// Different hostnames get different IPs.
func TestFakeIPPool_DistinctHosts(t *testing.T) {
	p := mustPool(t, "198.18.0.0/15", 30*time.Second)
	a, _, _ := p.Allocate("a.com")
	b, _, _ := p.Allocate("b.com")
	if a.Equal(b) {
		t.Fatalf("distinct hosts share IP %s", a)
	}
}

// An expired lease is reclaimed so the pool doesn't leak slots.
func TestFakeIPPool_ReclaimsExpired(t *testing.T) {
	// /30 = 4 addrs; offset 0 skipped → 3 usable slots.
	p := mustPool(t, "198.18.0.0/30", time.Nanosecond)
	first, _, ok := p.Allocate("h1")
	if !ok {
		t.Fatal("first allocate failed")
	}
	time.Sleep(time.Millisecond) // let the 1ns lease expire
	// Fill enough new hosts that the scan must wrap and reclaim first's slot.
	var reused bool
	for i := 0; i < 6; i++ {
		ip, _, ok := p.Allocate("new" + string(rune('a'+i)))
		if !ok {
			t.Fatalf("allocate new%d failed — expired slots not reclaimed", i)
		}
		if ip.Equal(first) {
			reused = true
		}
	}
	if !reused {
		t.Fatal("expired IP was never reused")
	}
}

// A pool with no free unexpired slots reports exhaustion instead of leaking.
func TestFakeIPPool_Exhaustion(t *testing.T) {
	p := mustPool(t, "198.18.0.0/30", time.Hour) // 3 usable slots, long lease
	for i := 0; i < 3; i++ {
		if _, _, ok := p.Allocate("h" + string(rune('a'+i))); !ok {
			t.Fatalf("allocate %d should succeed", i)
		}
	}
	if _, _, ok := p.Allocate("overflow"); ok {
		t.Fatal("expected exhaustion, got an allocation")
	}
}

func TestNewFakeIPPool_RejectsBadInput(t *testing.T) {
	if _, err := newFakeIPPool("nonsense", time.Second); err == nil {
		t.Fatal("expected error on bad CIDR")
	}
	if _, err := newFakeIPPool("2001:db8::/64", time.Second); err == nil {
		t.Fatal("expected error on IPv6 range")
	}
	if _, err := newFakeIPPool("198.18.0.0/32", time.Second); err == nil {
		t.Fatal("expected error on too-small range")
	}
}
