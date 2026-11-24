package pfctl

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type fakeRunner struct {
	mu        sync.Mutex
	plain     [][]string
	withIn    [][]string
	stdinSeen []string
	out       []byte
	err       error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain = append(f.plain, append([]string{name}, args...))
	return f.out, f.err
}

func (f *fakeRunner) RunStdin(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withIn = append(f.withIn, append([]string{name}, args...))
	f.stdinSeen = append(f.stdinSeen, string(stdin))
	return f.out, f.err
}

func TestManager_Enable_LoadsRulesIntoAnchor(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	if err := m.Enable(context.Background()); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !m.Enabled() {
		t.Errorf("expected Enabled() = true")
	}
	if len(r.withIn) != 1 {
		t.Fatalf("expected 1 stdin call, got %d", len(r.withIn))
	}
	args := strings.Join(r.withIn[0], " ")
	if !strings.Contains(args, "/sbin/pfctl -a em-wall -f -") {
		t.Errorf("unexpected pfctl invocation: %s", args)
	}
	body := r.stdinSeen[0]
	for _, want := range []string{"port 853", "1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !strings.Contains(body, want) {
			t.Errorf("rule body missing %q\n%s", want, body)
		}
	}
}

func TestManager_Disable_FlushesAnchor(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	_ = m.Enable(context.Background())
	if err := m.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if m.Enabled() {
		t.Errorf("expected Enabled() = false after Disable")
	}
	if len(r.plain) != 1 {
		t.Fatalf("expected 1 plain call, got %d", len(r.plain))
	}
	if !strings.Contains(strings.Join(r.plain[0], " "), "/sbin/pfctl -a em-wall -F all") {
		t.Errorf("unexpected disable call: %v", r.plain[0])
	}
}

func TestManager_Sync(t *testing.T) {
	r := &fakeRunner{}
	m := New(r)
	if err := m.Sync(context.Background(), true); err != nil {
		t.Fatalf("Sync(true): %v", err)
	}
	if !m.Enabled() {
		t.Errorf("expected enabled after Sync(true)")
	}
	if err := m.Sync(context.Background(), false); err != nil {
		t.Fatalf("Sync(false): %v", err)
	}
	if m.Enabled() {
		t.Errorf("expected disabled after Sync(false)")
	}
}
