package dnsproxy

import (
	"context"
	"io"
	"log"
	"net"
	"sort"
	"sync"
	"testing"
	"time"
)

func mustPool(t *testing.T, cidr string, ttl time.Duration) *fakeIPPool {
	t.Helper()
	return mustPoolStore(t, cidr, ttl, nil)
}

func mustPoolStore(t *testing.T, cidr string, ttl time.Duration, store FakeIPStore) *fakeIPPool {
	t.Helper()
	p, err := newFakeIPPool(cidr, ttl, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newFakeIPPool(%q): %v", cidr, err)
	}
	return p
}

// memFakeIPStore is an in-memory FakeIPStore standing in for the SQLite
// table, keyed by host with the same "one hostname per IP" invariant.
type memFakeIPStore struct {
	mu     sync.Mutex
	leases map[string]FakeIPLease
	puts   int
}

func newMemFakeIPStore() *memFakeIPStore {
	return &memFakeIPStore{leases: map[string]FakeIPLease{}}
}

func (m *memFakeIPStore) ListFakeIPLeases(_ context.Context, now time.Time) ([]FakeIPLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []FakeIPLease
	for _, l := range m.leases {
		if l.ExpiresAt.After(now) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func (m *memFakeIPStore) PutFakeIPLease(_ context.Context, l FakeIPLease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	for host, existing := range m.leases {
		if existing.IP == l.IP && host != l.Host {
			delete(m.leases, host)
		}
	}
	m.leases[l.Host] = l
	return nil
}

func (m *memFakeIPStore) DeleteFakeIPLease(_ context.Context, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, host)
	return nil
}

func (m *memFakeIPStore) DeleteExpiredFakeIPLeases(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, l := range m.leases {
		if !l.ExpiresAt.After(now) {
			delete(m.leases, host)
		}
	}
	return nil
}

func (m *memFakeIPStore) hostFor(ip string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, l := range m.leases {
		if l.IP == ip {
			return host
		}
	}
	return ""
}

func (m *memFakeIPStore) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

// A hostname always maps to the same IP while its lease is live, and the IP
// comes from the configured block.
func TestFakeIPPool_StablePerHost(t *testing.T) {
	p := mustPool(t, "198.18.0.0/15", 30*time.Second)
	_, block, _ := net.ParseCIDR("198.18.0.0/15")

	ip1, ok := p.Allocate("a.googlevideo.com")
	if !ok {
		t.Fatal("allocate failed")
	}
	if !block.Contains(ip1) {
		t.Fatalf("ip %s not in 198.18.0.0/15", ip1)
	}
	ip2, _ := p.Allocate("a.googlevideo.com")
	if !ip1.Equal(ip2) {
		t.Fatalf("same host got different IPs: %s vs %s", ip1, ip2)
	}
}

// Different hostnames get different IPs.
func TestFakeIPPool_DistinctHosts(t *testing.T) {
	p := mustPool(t, "198.18.0.0/15", 30*time.Second)
	a, _ := p.Allocate("a.com")
	b, _ := p.Allocate("b.com")
	if a.Equal(b) {
		t.Fatalf("distinct hosts share IP %s", a)
	}
}

// An expired lease is reclaimed so the pool doesn't leak slots.
func TestFakeIPPool_ReclaimsExpired(t *testing.T) {
	// /30 = 4 addrs; offset 0 skipped → 3 usable slots.
	p := mustPool(t, "198.18.0.0/30", time.Nanosecond)
	first, ok := p.Allocate("h1")
	if !ok {
		t.Fatal("first allocate failed")
	}
	time.Sleep(time.Millisecond) // let the 1ns lease expire
	// Fill enough new hosts that the scan must wrap and reclaim first's slot.
	var reused bool
	for i := 0; i < 6; i++ {
		ip, ok := p.Allocate("new" + string(rune('a'+i)))
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
		if _, ok := p.Allocate("h" + string(rune('a'+i))); !ok {
			t.Fatalf("allocate %d should succeed", i)
		}
	}
	if _, ok := p.Allocate("overflow"); ok {
		t.Fatal("expected exhaustion, got an allocation")
	}
}

func TestNewFakeIPPool_RejectsBadInput(t *testing.T) {
	disc := log.New(io.Discard, "", 0)
	if _, err := newFakeIPPool("nonsense", time.Second, nil, disc); err == nil {
		t.Fatal("expected error on bad CIDR")
	}
	if _, err := newFakeIPPool("2001:db8::/64", time.Second, nil, disc); err == nil {
		t.Fatal("expected error on IPv6 range")
	}
	if _, err := newFakeIPPool("198.18.0.0/32", time.Second, nil, disc); err == nil {
		t.Fatal("expected error on too-small range")
	}
}

// The regression this whole mechanism exists for: after a restart a client
// still holding a pre-restart fake IP must reach the SAME hostname. Without
// persistence the second pool restarts its scan at offset 0 and hands that
// address to whichever host queries first.
func TestFakeIPPool_SurvivesRestart(t *testing.T) {
	store := newMemFakeIPStore()

	first := mustPoolStore(t, "198.18.0.0/15", time.Hour, store)
	apIP, ok := first.Allocate("ap.spotify.com")
	if !ok {
		t.Fatal("allocate ap.spotify.com failed")
	}
	for _, h := range []string{"i.scdn.co", "daily-mix.scdn.co", "open.spotify.com"} {
		if _, ok := first.Allocate(h); !ok {
			t.Fatalf("allocate %s failed", h)
		}
	}

	// "Restart": brand-new pool over the same store.
	second := mustPoolStore(t, "198.18.0.0/15", time.Hour, store)
	second.restore(context.Background())

	// A different host queries first after the restart; it must not be
	// handed the address the AP still owns.
	other, ok := second.Allocate("daily-mix.scdn.co")
	if !ok {
		t.Fatal("allocate after restart failed")
	}
	if other.Equal(apIP) {
		t.Fatalf("daily-mix.scdn.co reused ap.spotify.com's IP %s across restart", apIP)
	}
	again, ok := second.Allocate("ap.spotify.com")
	if !ok {
		t.Fatal("re-allocate ap.spotify.com failed")
	}
	if !again.Equal(apIP) {
		t.Fatalf("ap.spotify.com moved across restart: %s → %s", apIP, again)
	}
	// A fresh hostname must land somewhere nobody holds.
	fresh, ok := second.Allocate("api.spotify.com")
	if !ok {
		t.Fatal("allocate fresh host failed")
	}
	if fresh.Equal(apIP) || fresh.Equal(other) {
		t.Fatalf("fresh host collided with a restored lease: %s", fresh)
	}
}

// Reclaiming an expired slot must drop the previous holder's row, or the
// stale binding comes back on the next restart.
func TestFakeIPPool_EvictionClearsStore(t *testing.T) {
	store := newMemFakeIPStore()
	p := mustPoolStore(t, "198.18.0.0/30", time.Nanosecond, store)

	first, ok := p.Allocate("old.example")
	if !ok {
		t.Fatal("first allocate failed")
	}
	time.Sleep(time.Millisecond)
	for i := 0; i < 6; i++ {
		if _, ok := p.Allocate("new" + string(rune('a'+i))); !ok {
			t.Fatalf("allocate new%d failed", i)
		}
	}
	if h := store.hostFor(first.String()); h == "old.example" {
		t.Fatalf("evicted host still owns %s in the store", first)
	}
	if _, still := store.leases["old.example"]; still {
		t.Fatal("evicted host still has a stored lease")
	}
}

// A hot hostname re-queried on the DNS answer's cadence must not write to
// the store on every query — only once the stored expiry has drifted.
func TestFakeIPPool_RefreshWritesAreThrottled(t *testing.T) {
	store := newMemFakeIPStore()
	p := mustPoolStore(t, "198.18.0.0/15", time.Hour, store)

	if _, ok := p.Allocate("hot.example"); !ok {
		t.Fatal("allocate failed")
	}
	if got := store.putCount(); got != 1 {
		t.Fatalf("first allocate wrote %d times, want 1", got)
	}
	for i := 0; i < 20; i++ {
		p.Allocate("hot.example")
	}
	if got := store.putCount(); got != 1 {
		t.Fatalf("refreshes wrote %d times, want 1 (throttle not applied)", got)
	}
}

// Release frees the slot in memory and in the store.
func TestFakeIPPool_ReleaseClearsStore(t *testing.T) {
	store := newMemFakeIPStore()
	p := mustPoolStore(t, "198.18.0.0/15", time.Hour, store)

	ip, ok := p.Allocate("doomed.example")
	if !ok {
		t.Fatal("allocate failed")
	}
	p.Release("doomed.example")
	if h := store.hostFor(ip.String()); h != "" {
		t.Fatalf("released IP still mapped to %q", h)
	}
	next, ok := p.Allocate("other.example")
	if !ok {
		t.Fatal("allocate after release failed")
	}
	if next.Equal(ip) && store.hostFor(ip.String()) != "other.example" {
		t.Fatal("store not updated after slot reuse")
	}
}

// Rows whose IP falls outside the configured block (range reconfigured
// between runs) are dropped rather than corrupting the pool.
func TestFakeIPPool_RestoreDropsOutOfRange(t *testing.T) {
	store := newMemFakeIPStore()
	future := time.Now().Add(time.Hour)
	_ = store.PutFakeIPLease(context.Background(), FakeIPLease{Host: "in.example", IP: "198.18.0.5", ExpiresAt: future})
	_ = store.PutFakeIPLease(context.Background(), FakeIPLease{Host: "out.example", IP: "10.0.0.5", ExpiresAt: future})
	_ = store.PutFakeIPLease(context.Background(), FakeIPLease{Host: "junk.example", IP: "not-an-ip", ExpiresAt: future})

	p := mustPoolStore(t, "198.18.0.0/15", time.Hour, store)
	p.restore(context.Background())

	ip, ok := p.Allocate("in.example")
	if !ok || ip.String() != "198.18.0.5" {
		t.Fatalf("in-range lease not restored: %v ok=%v", ip, ok)
	}
	if got, _ := p.Allocate("out.example"); got.String() == "10.0.0.5" {
		t.Fatal("out-of-range lease was restored")
	}
}
