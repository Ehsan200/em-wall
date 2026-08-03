package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
)

// newTestDeps builds the minimum handlerDeps the dynamic-group reconcile
// path touches: a real store + engine, a proxy table, and no router (route
// installs are skipped, which is what a non-root test wants anyway).
func newTestDeps(t *testing.T) *handlerDeps {
	t.Helper()
	store, err := rules.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &handlerDeps{
		store:      store,
		engine:     decision.New(store),
		proxyTable: proxy.NewTable(time.Minute),
	}
}

func addRule(t *testing.T, d *handlerDeps, pattern, iface string) rules.Rule {
	t.Helper()
	r, err := d.store.Add(context.Background(), rules.Rule{
		Pattern: pattern, Action: rules.ActionRoute, Interface: iface, Enabled: true,
	})
	if err != nil {
		t.Fatalf("add rule %s: %v", pattern, err)
	}
	return r
}

func patternSet(t *testing.T, d *handlerDeps) map[string]rules.Rule {
	t.Helper()
	all, err := d.store.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]rules.Rule{}
	for _, r := range all {
		out[r.Pattern] = r
	}
	return out
}

// The whole point of a dynamic group: when the vendor publishes a new
// prefix, the applied group grows a rule for it — inheriting the interface
// the user picked — and a withdrawn prefix loses its rule.
func TestReconcileDynamicGroupRules_AddsAndRemoves(t *testing.T) {
	d := newTestDeps(t)
	ctx := context.Background()

	before := []string{"142.250.0.0/15", "74.125.0.0/16"}
	addRule(t, d, "142.250.0.0/15", "xray:us")
	addRule(t, d, "74.125.0.0/16", "xray:us")

	next := []string{"142.250.0.0/15", "216.239.32.0/19"} // 74.125 withdrawn

	added, removed := d.reconcileDynamicGroupRules(ctx, before, next)
	if added != 1 || removed != 1 {
		t.Fatalf("got added=%d removed=%d, want 1/1", added, removed)
	}

	have := patternSet(t, d)
	if _, ok := have["216.239.32.0/19"]; !ok {
		t.Error("new vendor prefix did not become a rule")
	}
	if _, ok := have["74.125.0.0/16"]; ok {
		t.Error("withdrawn vendor prefix still has a rule")
	}
	if got := have["216.239.32.0/19"].Interface; got != "xray:us" {
		t.Errorf("new rule interface = %q, want the applied group's xray:us", got)
	}
}

// A group the user never applied has no rules; a refresh must not create any
// out of nowhere.
func TestReconcileDynamicGroupRules_NoOpWhenGroupNotApplied(t *testing.T) {
	d := newTestDeps(t)
	ctx := context.Background()
	addRule(t, d, "*.example.com", "xray:us") // unrelated domain rule

	added, removed := d.reconcileDynamicGroupRules(ctx,
		[]string{"142.250.0.0/15"}, []string{"142.250.0.0/15", "8.34.208.0/20"})
	if added != 0 || removed != 0 {
		t.Fatalf("got added=%d removed=%d, want 0/0", added, removed)
	}
	if len(patternSet(t, d)) != 1 {
		t.Error("rules were touched for an unapplied group")
	}
}

// Rules a disabled group owns stay disabled when the feed grows: the new
// prefix inherits the template rule's enabled state, not a default.
func TestReconcileDynamicGroupRules_InheritsDisabledState(t *testing.T) {
	d := newTestDeps(t)
	ctx := context.Background()
	r := addRule(t, d, "142.250.0.0/15", "xray:us")
	r.Enabled = false
	if err := d.store.Update(ctx, r); err != nil {
		t.Fatalf("update: %v", err)
	}

	if added, _ := d.reconcileDynamicGroupRules(ctx,
		[]string{"142.250.0.0/15"}, []string{"142.250.0.0/15", "74.125.0.0/16"}); added != 1 {
		t.Fatalf("expected 1 added")
	}
	if got := patternSet(t, d)["74.125.0.0/16"]; got.Enabled {
		t.Error("new rule should have inherited enabled=false")
	}
}

// The cached feed replaces the seed as the group's pattern list, and a group
// with no cache yet still resolves to its seed.
func TestEffectiveGroupPatterns_CachePreferredOverSeed(t *testing.T) {
	d := newTestDeps(t)
	ctx := context.Background()
	g := groups.FindByKey("google-media")
	if g == nil {
		t.Fatal("google-media group missing")
	}

	seed := d.effectiveGroupPatterns(ctx, *g)
	if len(seed) != len(g.Patterns) {
		t.Fatalf("without a cache the seed must be used, got %d patterns", len(seed))
	}

	d.saveDynamicGroupCache(ctx, g.Key, dynamicGroupCache{
		Patterns: []string{"1.2.3.0/24"}, FetchedAt: time.Now(),
	})
	live := d.effectiveGroupPatterns(ctx, *g)
	if len(live) != 1 || live[0] != "1.2.3.0/24" {
		t.Fatalf("cached feed should win, got %v", live)
	}

	// Static groups are unaffected by any of this.
	static := groups.FindByKey("anthropic")
	if got := d.effectiveGroupPatterns(ctx, *static); len(got) != len(static.Patterns) {
		t.Error("static group patterns changed")
	}
}

func TestDynamicGroupCache_Due(t *testing.T) {
	now := time.Now()
	if !(dynamicGroupCache{}).due(now, time.Hour) {
		t.Error("never-fetched cache must be due")
	}
	fresh := dynamicGroupCache{Patterns: []string{"1.2.3.0/24"}, FetchedAt: now.Add(-time.Minute)}
	if fresh.due(now, time.Hour) {
		t.Error("recently fetched cache must not be due")
	}
	if !(dynamicGroupCache{Patterns: []string{"1.2.3.0/24"}, FetchedAt: now.Add(-2 * time.Hour)}).due(now, time.Hour) {
		t.Error("stale cache must be due")
	}
	// A failed fetch retries on the shorter window, but not immediately.
	failed := dynamicGroupCache{Patterns: []string{"1.2.3.0/24"}, FetchedAt: now.Add(-time.Minute), Error: "boom"}
	if failed.due(now, 24*time.Hour) {
		t.Error("a just-failed fetch must not retry instantly")
	}
	failed.FetchedAt = now.Add(-dynGroupRetryAfter - time.Minute)
	if !failed.due(now, 24*time.Hour) {
		t.Error("a failed fetch must retry after the retry window")
	}
}
