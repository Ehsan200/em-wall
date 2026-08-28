package main

import (
	"context"
	"time"

	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/rules"
)

// fakeIPStore adapts *rules.Store to dnsproxy.FakeIPStore. The conversion
// lives here rather than in core/rules because core/decision already
// imports core/rules, so rules importing dnsproxy would close a cycle.
type fakeIPStore struct{ store *rules.Store }

func (f fakeIPStore) ListFakeIPLeases(ctx context.Context, now time.Time) ([]dnsproxy.FakeIPLease, error) {
	rows, err := f.store.ListFakeIPLeases(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]dnsproxy.FakeIPLease, 0, len(rows))
	for _, r := range rows {
		out = append(out, dnsproxy.FakeIPLease{Host: r.Host, IP: r.IP, ExpiresAt: r.ExpiresAt})
	}
	return out, nil
}

func (f fakeIPStore) PutFakeIPLease(ctx context.Context, l dnsproxy.FakeIPLease) error {
	return f.store.PutFakeIPLease(ctx, rules.FakeIPLease{Host: l.Host, IP: l.IP, ExpiresAt: l.ExpiresAt})
}

func (f fakeIPStore) DeleteFakeIPLease(ctx context.Context, host string) error {
	return f.store.DeleteFakeIPLease(ctx, host)
}

func (f fakeIPStore) DeleteExpiredFakeIPLeases(ctx context.Context, now time.Time) error {
	return f.store.DeleteExpiredFakeIPLeases(ctx, now)
}
