package routing

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := append([]string{name}, args...)
	f.calls = append(f.calls, c)
	return f.out, f.err
}

func (f *fakeRunner) joinedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestManager_InstallV4(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	ctx := context.Background()
	if err := m.Install(ctx, "1.2.3.4", "utun3", time.Minute, 7); err != nil {
		t.Fatalf("Install: %v", err)
	}
	calls := r.joinedCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "/sbin/route -n add -host 1.2.3.4 -interface utun3") {
		t.Errorf("unexpected call: %v", calls)
	}
	if strings.Contains(calls[0], "-inet6") {
		t.Errorf("v4 install should not include -inet6: %v", calls[0])
	}
	if got := m.Active(); len(got) != 1 || got[0].Host != "1.2.3.4" || got[0].Interface != "utun3" || got[0].RuleID != 7 {
		t.Errorf("Active() = %+v", got)
	}
}

func TestManager_InstallV6(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	if err := m.Install(context.Background(), "2606:4700:4700::1111", "utun3", time.Minute, 0); err != nil {
		t.Fatalf("Install: %v", err)
	}
	calls := r.joinedCalls()
	if !strings.Contains(calls[0], "-inet6") {
		t.Errorf("v6 install should include -inet6: %v", calls[0])
	}
}

func TestManager_RejectsBadIP(t *testing.T) {
	m := New(&fakeRunner{})
	if err := m.Install(context.Background(), "not-an-ip", "utun3", time.Minute, 0); err == nil {
		t.Errorf("expected error for invalid IP")
	}
	if err := m.Install(context.Background(), "1.2.3.4", "", time.Minute, 0); err == nil {
		t.Errorf("expected error for empty interface")
	}
}

func TestManager_ReinstallReplacesRoute(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	ctx := context.Background()
	_ = m.Install(ctx, "1.2.3.4", "utun3", time.Minute, 1)
	_ = m.Install(ctx, "1.2.3.4", "utun9", time.Minute, 1)
	calls := r.joinedCalls()
	// expect: add utun3, delete, add utun9
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls (add, delete, add), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[1], "delete") {
		t.Errorf("expected second call to be delete, got %v", calls[1])
	}
	if !strings.Contains(calls[2], "utun9") {
		t.Errorf("expected third call to use utun9, got %v", calls[2])
	}
}

func TestManager_Remove(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	ctx := context.Background()
	_ = m.Install(ctx, "1.2.3.4", "utun3", time.Minute, 0)
	_ = m.Remove(ctx, "1.2.3.4")
	if len(m.Active()) != 0 {
		t.Errorf("expected no active routes after Remove")
	}
	if err := m.Remove(ctx, "9.9.9.9"); err != nil {
		t.Errorf("removing unknown host should be no-op, got %v", err)
	}
}

func TestManager_RemoveByRule(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	ctx := context.Background()
	_ = m.Install(ctx, "1.2.3.4", "utun3", time.Minute, 1)
	_ = m.Install(ctx, "5.6.7.8", "utun3", time.Minute, 1)
	_ = m.Install(ctx, "9.9.9.9", "utun3", time.Minute, 2)

	if err := m.RemoveByRule(ctx, 1); err != nil {
		t.Fatal(err)
	}
	active := m.Active()
	if len(active) != 1 || active[0].RuleID != 2 {
		t.Errorf("expected only rule 2 to remain, got %+v", active)
	}
}

func TestManager_SweepExpired(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	now := time.Now()
	m.now = func() time.Time { return now }
	ctx := context.Background()
	_ = m.Install(ctx, "1.2.3.4", "utun3", time.Second, 0)
	_ = m.Install(ctx, "5.6.7.8", "utun3", time.Hour, 0)

	m.now = func() time.Time { return now.Add(2 * time.Second) }
	n := m.SweepExpired(ctx)
	if n != 1 {
		t.Errorf("SweepExpired = %d, want 1", n)
	}
	active := m.Active()
	if len(active) != 1 || active[0].Host != "5.6.7.8" {
		t.Errorf("expected only 5.6.7.8 to remain, got %+v", active)
	}
}

func TestParseNetstat(t *testing.T) {
	sample := `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            192.168.35.1       UGScg                 en0
127                127.0.0.1          UCS                   lo0
1.1.1.1/32         link#31            UCS                 utun9
192.168.35         link#15            UCS                   en0      !

Internet6:
Destination                             Gateway                         Flags               Netif Expire
default                                 fe80::1%utun9                   UGcg                utun9
::1                                     ::1                             UHL                   lo0
`
	routes := parseNetstat([]byte(sample))
	if len(routes) < 5 {
		t.Fatalf("expected several rows, got %d: %+v", len(routes), routes)
	}

	var sawDefault, sawUtun, sawV6 bool
	for _, r := range routes {
		if r.Family == "inet" && r.Destination == "default" && r.Interface == "en0" {
			sawDefault = true
		}
		if r.Family == "inet" && r.Destination == "1.1.1.1/32" && r.Interface == "utun9" {
			sawUtun = true
		}
		if r.Family == "inet6" && r.Destination == "default" && r.Interface == "utun9" {
			sawV6 = true
		}
	}
	if !sawDefault {
		t.Errorf("did not parse default v4 route via en0")
	}
	if !sawUtun {
		t.Errorf("did not parse 1.1.1.1/32 via utun9")
	}
	if !sawV6 {
		t.Errorf("did not parse v6 default via utun9")
	}
}

func TestManager_Flush(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	ctx := context.Background()
	_ = m.Install(ctx, "1.2.3.4", "utun3", time.Hour, 0)
	_ = m.Install(ctx, "5.6.7.8", "utun3", time.Hour, 0)
	m.Flush(ctx)
	if len(m.Active()) != 0 {
		t.Errorf("expected empty after Flush, got %+v", m.Active())
	}
}
