package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// newSetTestDeps is newTestDeps plus a real xray store, which the set
// paths need for expansion and existence checks.
func newSetTestDeps(t *testing.T) *handlerDeps {
	t.Helper()
	d := newTestDeps(t)
	xs, err := xray.Open(filepath.Join(t.TempDir(), "xray.db"))
	if err != nil {
		t.Fatalf("open xray store: %v", err)
	}
	t.Cleanup(func() { _ = xs.Close() })
	d.xrayStore = xs
	d.engine = decision.New(d.store)
	d.engine.SetExpander(xs)
	return d
}

func addXrayEntry(t *testing.T, d *handlerDeps, name string) {
	t.Helper()
	if _, err := d.xrayStore.Add(context.Background(), xray.Config{
		Name: name, Outbound: `{"protocol":"freedom"}`, Enabled: true,
	}); err != nil {
		t.Fatalf("add xray entry %s: %v", name, err)
	}
}

// A rule bound to a set must resolve through the engine, and must
// follow the set when its membership changes — with no touch to the
// rule row itself.
func TestSetBoundRuleFollowsMembershipEdits(t *testing.T) {
	ctx := context.Background()
	d := newSetTestDeps(t)
	addXrayEntry(t, d, "a")
	addXrayEntry(t, d, "b")
	addXrayEntry(t, d, "c")

	set, err := d.xrayStore.AddSet(ctx, xray.Set{Name: "iproute", Members: "xray:a,xray:b", Enabled: true})
	if err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	addRule(t, d, "example.com", xray.SetInterface("iproute"))
	if err := d.engine.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := d.engine.Decide("example.com").Interface; got != "xray:a,b" {
		t.Fatalf("iface = %q, want %q", got, "xray:a,b")
	}

	// Swap a member out. The rule row is untouched.
	set.Members = "xray:c,xray:b"
	if err := d.xrayStore.UpdateSet(ctx, set); err != nil {
		t.Fatalf("UpdateSet: %v", err)
	}
	if err := d.engine.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := d.engine.Decide("example.com").Interface; got != "xray:c,b" {
		t.Errorf("iface after edit = %q, want %q", got, "xray:c,b")
	}
	stored, err := d.store.List(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if stored[0].Interface != "xrayset:iproute" {
		t.Errorf("stored rule iface = %q, want the untouched reference", stored[0].Interface)
	}
}

// Disabling a set must make its rules fail closed, not fall back to the
// default route.
func TestDisabledSetFailsClosed(t *testing.T) {
	ctx := context.Background()
	d := newSetTestDeps(t)
	addXrayEntry(t, d, "a")

	set, err := d.xrayStore.AddSet(ctx, xray.Set{Name: "s1", Members: "xray:a", Enabled: true})
	if err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	addRule(t, d, "example.com", xray.SetInterface("s1"))

	set.Enabled = false
	if err := d.xrayStore.UpdateSet(ctx, set); err != nil {
		t.Fatalf("UpdateSet: %v", err)
	}
	if err := d.engine.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	dec := d.engine.Decide("example.com")
	if dec.Outcome != decision.OutcomeRoute {
		t.Fatalf("outcome = %v, want route", dec.Outcome)
	}
	// The raw ref survives: no routing-layer code matches it, so the
	// query is refused rather than silently escaping the tunnel.
	if dec.Interface != "xrayset:s1" {
		t.Errorf("iface = %q, want the unresolved raw ref", dec.Interface)
	}
}

func TestValidateSetRefsAndRuleReferences(t *testing.T) {
	ctx := context.Background()
	d := newSetTestDeps(t)
	addXrayEntry(t, d, "a")

	if err := d.validateSetRefs(ctx, "xrayset:nope"); err == nil {
		t.Error("validateSetRefs accepted an unknown set")
	}
	if err := d.validateSetRefs(ctx, "xray:a"); err != nil {
		t.Errorf("validateSetRefs rejected a non-set interface: %v", err)
	}

	if _, err := d.xrayStore.AddSet(ctx, xray.Set{Name: "s1", Members: "xray:a", Enabled: true}); err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	if err := d.validateSetRefs(ctx, "xrayset:s1"); err != nil {
		t.Errorf("validateSetRefs rejected a known set: %v", err)
	}

	addRule(t, d, "example.com", xray.SetInterface("s1"))
	ids, err := d.rulesReferencingSet(ctx, "s1")
	if err != nil {
		t.Fatalf("rulesReferencingSet: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("rulesReferencingSet = %v, want 1 rule", ids)
	}

	// The entry is a set member, so deleting it must be blocked.
	blockers, err := d.setMemberBlockers(ctx, xray.DialerKindXray, "a")
	if err != nil {
		t.Fatalf("setMemberBlockers: %v", err)
	}
	if len(blockers) != 1 || blockers[0] != "s1" {
		t.Errorf("setMemberBlockers = %v, want [s1]", blockers)
	}
}

// The cold-path readers (IP-route reconcile, latency probing) resolve
// sets for themselves, since they read store rows rather than the
// engine's cache.
func TestExpandRuleIfaces(t *testing.T) {
	ctx := context.Background()
	d := newSetTestDeps(t)
	addXrayEntry(t, d, "a")
	if _, err := d.xrayStore.AddSet(ctx, xray.Set{Name: "s1", Members: "xray:a", Enabled: true}); err != nil {
		t.Fatalf("AddSet: %v", err)
	}

	in := []rules.Rule{
		{Interface: "xrayset:s1"},
		{Interface: "utun4"},
		{Interface: "xrayset:gone"},
	}
	out := d.expandRuleIfaces(ctx, in)
	if out[0].Interface != "xray:a" {
		t.Errorf("expanded[0] = %q, want %q", out[0].Interface, "xray:a")
	}
	if out[1].Interface != "utun4" {
		t.Errorf("expanded[1] = %q, want it untouched", out[1].Interface)
	}
	if out[2].Interface != "xrayset:gone" {
		t.Errorf("expanded[2] = %q, want the raw ref", out[2].Interface)
	}
	// Input must not be mutated — callers keep using their own slice.
	if in[0].Interface != "xrayset:s1" {
		t.Error("expandRuleIfaces mutated its input")
	}
}

// Renaming a set must carry its rules along, and land them on the
// canonical (lowercased) name the expansion map is keyed by.
func TestSetRenameCascadesToRules(t *testing.T) {
	ctx := context.Background()
	d := newSetTestDeps(t)
	addXrayEntry(t, d, "a")

	set, err := d.xrayStore.AddSet(ctx, xray.Set{Name: "old", Members: "xray:a", Enabled: true})
	if err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	addRule(t, d, "example.com", xray.SetInterface("old"))

	// Rename through the store, then cascade the way the handler does —
	// using the canonical stored name, not the raw user input.
	set.Name = "NewName"
	if err := d.xrayStore.UpdateSet(ctx, set); err != nil {
		t.Fatalf("UpdateSet: %v", err)
	}
	renamed, err := d.xrayStore.GetSet(ctx, set.ID)
	if err != nil {
		t.Fatalf("GetSet: %v", err)
	}
	if _, err := d.store.RenameInterfaceRef(ctx, xray.SetPrefix, "old", renamed.Name); err != nil {
		t.Fatalf("RenameInterfaceRef: %v", err)
	}
	if err := d.engine.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	stored, err := d.store.List(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if stored[0].Interface != "xrayset:newname" {
		t.Fatalf("rule iface after rename = %q, want %q", stored[0].Interface, "xrayset:newname")
	}
	if got := d.engine.Decide("example.com").Interface; got != "xray:a" {
		t.Errorf("renamed set no longer resolves: iface = %q", got)
	}
}
