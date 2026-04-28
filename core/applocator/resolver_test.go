package applocator

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProvider struct {
	mu    sync.Mutex
	state map[string]string
}

func (f *fakeProvider) LsofUtunOwners() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.state))
	for k, v := range f.state {
		out[k] = v
	}
	return out
}

func (f *fakeProvider) set(state map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

func TestRefresh_DetectsNewApp(t *testing.T) {
	p := &fakeProvider{state: map[string]string{}}
	r := NewResolver(p)
	if got := r.Refresh(); len(got) != 0 {
		t.Errorf("expected no changes initially, got %+v", got)
	}
	p.set(map[string]string{"utun4": "v2box"})
	changes := r.Refresh()
	var v2box *AppChange
	for i := range changes {
		if changes[i].Key == "v2box" {
			v2box = &changes[i]
		}
	}
	if v2box == nil {
		t.Fatalf("expected v2box change, got %+v", changes)
	}
	if v2box.Old != "" || v2box.New != "utun4" {
		t.Errorf("got %+v", v2box)
	}
	if r.Current("v2box") != "utun4" {
		t.Errorf("Current() not updated")
	}
}

func TestRefresh_DetectsInterfaceChange(t *testing.T) {
	p := &fakeProvider{state: map[string]string{"utun4": "v2box"}}
	r := NewResolver(p)
	r.Refresh()

	p.set(map[string]string{"utun7": "v2box"})
	changes := r.Refresh()
	var ch *AppChange
	for i := range changes {
		if changes[i].Key == "v2box" {
			ch = &changes[i]
		}
	}
	if ch == nil || ch.Old != "utun4" || ch.New != "utun7" {
		t.Errorf("expected v2box utun4→utun7, got %+v", changes)
	}
}

func TestRefresh_DetectsAppGone(t *testing.T) {
	p := &fakeProvider{state: map[string]string{"utun4": "v2box"}}
	r := NewResolver(p)
	r.Refresh()
	p.set(map[string]string{})
	changes := r.Refresh()
	var ch *AppChange
	for i := range changes {
		if changes[i].Key == "v2box" {
			ch = &changes[i]
		}
	}
	if ch == nil || ch.Old != "utun4" || ch.New != "" {
		t.Errorf("expected v2box utun4→\"\", got %+v", changes)
	}
}

func TestAcquireForRead_Blocks_DuringWrite(t *testing.T) {
	p := &fakeProvider{}
	r := NewResolver(p)

	// Take write lock; hold for 80ms.
	releaseW := r.AcquireForWrite("v2box")
	released := atomic.Bool{}
	go func() {
		time.Sleep(80 * time.Millisecond)
		releaseW()
		released.Store(true)
	}()

	start := time.Now()
	releaseR, ok := r.AcquireForRead("v2box", 500*time.Millisecond)
	if !ok {
		t.Fatalf("expected to acquire read lock within 500ms")
	}
	defer releaseR()
	elapsed := time.Since(start)
	if elapsed < 70*time.Millisecond {
		t.Errorf("read should have waited for the writer, elapsed=%v", elapsed)
	}
	if !released.Load() {
		t.Errorf("writer should have released by now")
	}
}

func TestAcquireForRead_TimesOut(t *testing.T) {
	p := &fakeProvider{}
	r := NewResolver(p)

	// Hold write lock for the entire test.
	release := r.AcquireForWrite("v2box")
	defer release()

	releaseR, ok := r.AcquireForRead("v2box", 50*time.Millisecond)
	if ok {
		releaseR()
		t.Errorf("expected timeout, got read lock")
	}
}

func TestAcquireForRead_ConcurrentReadersOK(t *testing.T) {
	p := &fakeProvider{}
	r := NewResolver(p)

	var wg sync.WaitGroup
	wg.Add(10)
	failed := atomic.Bool{}
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			release, ok := r.AcquireForRead("v2box", 100*time.Millisecond)
			if !ok {
				failed.Store(true)
				return
			}
			defer release()
			time.Sleep(20 * time.Millisecond)
		}()
	}
	wg.Wait()
	if failed.Load() {
		t.Errorf("concurrent readers should not block each other")
	}
}

func TestKnownApps_HasExpected(t *testing.T) {
	apps := KnownApps()
	want := []string{"v2box", "v2raytun", "hiddify", "streisand", "tailscale", "wireguard"}
	have := map[string]bool{}
	for _, a := range apps {
		have[a.Key] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("registry missing %q", w)
		}
	}
}

func TestApp_MatchesProcess(t *testing.T) {
	a := App{Processes: []string{"v2box"}}
	if !a.MatchesProcess("v2box") {
		t.Errorf("exact match failed")
	}
	if !a.MatchesProcess("V2BOX") {
		t.Errorf("case-insensitive match failed")
	}
	if !a.MatchesProcess("v2box-helper") {
		t.Errorf("substring match failed")
	}
	if a.MatchesProcess("openvpn") {
		t.Errorf("unrelated process matched")
	}
}
