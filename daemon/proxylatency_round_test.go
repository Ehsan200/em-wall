package main

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
)

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	n, _ := strconv.Atoi(p)
	return n
}

func probeStore(t *testing.T, names ...string) *proxy.Store {
	t.Helper()
	st, err := proxy.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, n := range names {
		if _, err := st.Add(context.Background(), proxy.Proxy{
			Name: n, Protocol: proxy.ProtocolSOCKS5, Host: "127.0.0.1", Port: closedPort(t),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// A round where every upstream fails is a local outage: nobody is struck.
func TestProbeRoundAllFailRecordsNothing(t *testing.T) {
	st := probeStore(t, "a", "b")
	tr := netprobe.NewLatencyTracker(time.Minute)
	probeProxies(context.Background(), st, tr, []string{"a", "b"}, "example.com", 443)
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("all-fail round recorded %+v, want nothing", snap)
	}
}

// With a single name there is no sibling to compare against, so a failure
// is recorded as before.
func TestProbeRoundSingleFailureRecorded(t *testing.T) {
	st := probeStore(t, "a")
	tr := netprobe.NewLatencyTracker(time.Minute)
	probeProxies(context.Background(), st, tr, []string{"a"}, "example.com", 443)
	if snap := tr.Snapshot(); len(snap) != 1 || snap[0].Fails != 1 {
		t.Fatalf("snapshot = %+v, want one failure for a", snap)
	}
}
