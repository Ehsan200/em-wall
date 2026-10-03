package main

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

func newBulkDialerDeps(t *testing.T) *handlerDeps {
	t.Helper()
	d := newSetTestDeps(t)
	ps, err := proxy.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("open proxy store: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	d.proxyStore = ps
	d.xraySup = &xraySupervisor{xrayStore: d.xrayStore, proxyStore: ps, logger: log.New(io.Discard, "", 0)}
	return d
}

func xrayIDs(t *testing.T, d *handlerDeps, names ...string) []int64 {
	t.Helper()
	all, err := d.xrayStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, n := range names {
		for _, c := range all {
			if c.Name == n {
				ids = append(ids, c.ID)
			}
		}
	}
	return ids
}

func dialerOf(t *testing.T, d *handlerDeps, name string) string {
	t.Helper()
	all, _ := d.xrayStore.List(context.Background())
	for _, c := range all {
		if c.Name == name {
			return c.Dialer
		}
	}
	t.Fatalf("no entry %s", name)
	return ""
}

func TestBulkDialerModes(t *testing.T) {
	ctx := context.Background()
	d := newBulkDialerDeps(t)
	for _, n := range []string{"a", "b", "hop1", "hop2"} {
		addXrayEntry(t, d, n)
	}
	ids := xrayIDs(t, d, "a", "b")

	r, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: ids, Mode: xray.DialerEditReplace, Dialer: "xray:hop1"})
	if err != nil || r.Updated != 2 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	if _, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: ids[:1], Mode: xray.DialerEditAdd, Dialer: "xray:hop2,xray:hop1"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := dialerOf(t, d, "a"); got != "xray:hop1,xray:hop2" {
		t.Errorf("a after add = %q", got)
	}
	r, err = d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: ids, Mode: xray.DialerEditRemove, Dialer: "xray:hop1"})
	if err != nil || r.Updated != 2 {
		t.Fatalf("remove: %+v, %v", r, err)
	}
	if a, b := dialerOf(t, d, "a"), dialerOf(t, d, "b"); a != "xray:hop2" || b != "" {
		t.Errorf("after remove: a=%q b=%q", a, b)
	}
}

func TestBulkDialerRejectsWholeBatch(t *testing.T) {
	ctx := context.Background()
	d := newBulkDialerDeps(t)
	for _, n := range []string{"a", "b", "c"} {
		addXrayEntry(t, d, n)
	}
	// b already dials through a; pointing a and c at b closes a→b→a.
	if _, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: xrayIDs(t, d, "b"), Mode: xray.DialerEditReplace, Dialer: "xray:a"}); err != nil {
		t.Fatal(err)
	}
	_, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: xrayIDs(t, d, "c", "a"), Mode: xray.DialerEditReplace, Dialer: "xray:b"})
	if !errors.Is(err, xray.ErrDialerCycle) {
		t.Fatalf("cycle: err = %v", err)
	}
	if got := dialerOf(t, d, "c"); got != "" {
		t.Errorf("c written despite rejected batch: %q", got)
	}
	// Selecting the target itself is refused, not silently skipped.
	if _, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: xrayIDs(t, d, "a", "c"), Mode: xray.DialerEditAdd, Dialer: "xray:c"}); !errors.Is(err, xray.ErrDialerCycle) {
		t.Errorf("self ref: err = %v", err)
	}
	// Unknown refs are refused on add/replace...
	if _, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: xrayIDs(t, d, "c"), Mode: xray.DialerEditAdd, Dialer: "xraysub:nope"}); err == nil {
		t.Error("unknown sub ref accepted")
	}
	// ...but removing a dangling one is how it gets cleaned up.
	if err := d.xrayStore.SetDialers(ctx, map[int64]string{xrayIDs(t, d, "c")[0]: "xraysub:gone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.bulkEditDialer(ctx, ipc.XrayBulkDialerParams{IDs: xrayIDs(t, d, "c"), Mode: xray.DialerEditRemove, Dialer: "xraysub:gone"}); err != nil {
		t.Errorf("remove dangling: %v", err)
	}
}
