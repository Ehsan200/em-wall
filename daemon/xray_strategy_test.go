package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

func agileSlot(keys ...string) xray.DialerSlot {
	sl := xray.DialerSlot{Master: "m", Index: 0, PoolKey: "xraysub:p", Strategy: xray.StrategyAgile}
	for _, k := range keys {
		sl.Members = append(sl.Members, xray.DialerMember{Key: k})
	}
	return sl
}

func tagged(sl xray.DialerSlot, st map[string]nodeStatus) map[string]nodeStatus {
	out := map[string]nodeStatus{}
	for k, v := range st {
		out[xray.SlotMemberTag(sl.Index, k)] = v
	}
	return out
}

func TestPickAgile(t *testing.T) {
	sl := agileSlot("a", "b", "c", "d", "e", "f", "g")
	st := map[string]nodeStatus{
		"a": ping(400, 10, 6, 0), "b": ping(100, 10, 6, 0), "c": ping(200, 10, 6, 0),
		"d": ping(300, 10, 6, 0), "e": ping(500, 10, 6, 0), "f": ping(600, 10, 6, 0),
		"g": ping(50, 5, 6, 4), // fast but failing beyond tolerance: not alive
	}
	got, ok := pickAgile(sl, tagged(sl, st), agilePick{})
	if !ok || !reflect.DeepEqual(got.active, []string{"a", "b", "c", "d"}) || got.spare != "e" {
		t.Fatalf("pick = %+v %v, want active a..d spare e", got, ok)
	}
	// Sticky: e at 380ms doesn't displace incumbent a at 400ms…
	st["e"] = ping(380, 10, 6, 0)
	if again, _ := pickAgile(sl, tagged(sl, st), got); !reflect.DeepEqual(again.active, got.active) {
		t.Errorf("near-equal challenger moved the pick: %v", again.active)
	}
	// …but a clearly faster one does.
	st["e"] = ping(90, 10, 6, 0)
	if again, _ := pickAgile(sl, tagged(sl, st), got); !reflect.DeepEqual(again.active, []string{"b", "c", "d", "e"}) {
		t.Errorf("clearly faster node not picked: %v", again.active)
	}
	// Nothing alive: keep whatever is loaded.
	dead := map[string]nodeStatus{}
	for k := range st {
		dead[k] = ping(0, 0, 6, 6)
	}
	if _, ok := pickAgile(sl, tagged(sl, dead), got); ok {
		t.Error("picked with every node dead")
	}
	// Fewer live than the cap: no spare to hand, the fallback stays put.
	two := map[string]nodeStatus{"a": ping(100, 5, 6, 0), "b": ping(90, 5, 6, 0)}
	first, _ := pickAgile(sl, tagged(sl, two), agilePick{})
	two["a"] = ping(80, 5, 6, 0)
	second, _ := pickAgile(sl, tagged(sl, two), first)
	if first.spare != "b" || second.spare != "b" {
		t.Errorf("fallback moved on noise: %q → %q", first.spare, second.spare)
	}
}

func TestAgilePickerDropped(t *testing.T) {
	p := newAgilePicker()
	sl := agileSlot("a", "b", "c")
	p.observe([]xray.DialerSlot{sl}, tagged(sl, map[string]nodeStatus{
		"a": ping(100, 5, 6, 0), "b": ping(200, 5, 6, 0), "c": ping(300, 5, 6, 0),
	}))
	ch := p.observe([]xray.DialerSlot{sl}, tagged(sl, map[string]nodeStatus{
		"a": ping(0, 0, 6, 6), "b": ping(200, 5, 6, 0), "c": ping(300, 5, 6, 0),
	}))
	if len(ch) != 1 || !reflect.DeepEqual(ch[0].dropped, []string{"a"}) || !reflect.DeepEqual(ch[0].to.active, []string{"b", "c"}) {
		t.Fatalf("changes = %+v", ch)
	}
	// The pool leaves agile: its pick is forgotten.
	sl.Strategy = xray.StrategyStable
	p.observe([]xray.DialerSlot{sl}, nil)
	if _, ok := p.get(sl.PoolKey); ok {
		t.Error("pick kept for a pool that is no longer agile")
	}
}

func TestAutoClassifier(t *testing.T) {
	c := newAutoClassifier()
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	calm := ipc.PoolTimelineDTO{Nodes: []ipc.PoolNodeTimelineDTO{{States: "AAAA"}, {States: "AAAA"}, {States: "DDDD"}}}
	wavy := ipc.PoolTimelineDTO{Nodes: []ipc.PoolNodeTimelineDTO{
		{States: "ADAD", Flips: 3}, {States: "DADA", Flips: 3}, {States: "AAAA"}, {States: "AAAA"}, {States: "    "},
	}}
	auto := map[string]bool{"k": true}

	if ev := c.evaluate(map[string]ipc.PoolTimelineDTO{"k": calm}, auto); len(ev) != 0 || c.agile("k") {
		t.Fatalf("calm pool switched: %+v", ev)
	}
	ev := c.evaluate(map[string]ipc.PoolTimelineDTO{"k": wavy}, auto)
	if len(ev) != 1 || !ev[0].agile || !c.agile("k") {
		t.Fatalf("flapping pool not agile: %+v", ev)
	}
	// 2 of 4 observed nodes flipped twice+ (the never-measured one doesn't count).
	if ev[0].reason != "2/4 nodes flipped up↔down 2+ times in 10m" {
		t.Errorf("reason = %q", ev[0].reason)
	}
	// Calm, but not for long enough: stays agile.
	now = now.Add(autoCalm - time.Minute)
	if ev := c.evaluate(map[string]ipc.PoolTimelineDTO{"k": calm}, auto); len(ev) != 0 || !c.agile("k") {
		t.Errorf("left agile before the calm period: %+v", ev)
	}
	now = now.Add(time.Minute)
	if ev := c.evaluate(map[string]ipc.PoolTimelineDTO{"k": calm}, auto); len(ev) != 1 || ev[0].agile || c.agile("k") {
		t.Errorf("didn't return to stable after the calm period: %+v", ev)
	}
	// Two pick losses alone are flapping too.
	if f, why := flapping(ipc.PoolTimelineDTO{PickLosses: 2}); !f || why != "whole pick died 2× in 10m" {
		t.Errorf("pick losses: %v %q", f, why)
	}
	// A pool no longer on auto is forgotten.
	c.evaluate(nil, nil)
	if _, ok := c.state("k"); ok {
		t.Error("state kept for a pool that left auto")
	}
}

// newStrategyRig is a supervisor over a real store with one subscription
// "pool" of n nodes and a master "m" dialing through it.
func newStrategyRig(t *testing.T, n int) (*xraySupervisor, *xray.Store, xray.Subscription, []string) {
	t.Helper()
	ctx := context.Background()
	xs, err := xray.Open(filepath.Join(t.TempDir(), "xray.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = xs.Close() })
	sub, err := xs.AddSub(ctx, xray.Subscription{Name: "pool", URL: "https://example.invalid/sub", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []xray.SubNode
	var fps []string
	for i := 0; i < n; i++ {
		ob := fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":"n%d.example","port":443,"users":[{"id":"x"}]}]}}`, i)
		fp := xray.Fingerprint(ob)
		fps = append(fps, fp)
		nodes = append(nodes, xray.SubNode{Name: fmt.Sprintf("n%d", i), Fingerprint: fp, Outbound: ob})
	}
	if err := xs.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	if _, err := xs.Add(ctx, xray.Config{
		Name: "m", Enabled: true, Dialer: "xraysub:pool",
		Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"exit.example","port":443,"users":[{"id":"y"}]}]}}`,
	}); err != nil {
		t.Fatal(err)
	}
	s := &xraySupervisor{
		xrayStore: xs, logger: log.New(io.Discard, "", 0),
		parker: newNodeParker(), shortlist: newSlotShortlist(), timeline: newPoolTimeline(),
		agile: newAgilePicker(), auto: newAutoClassifier(),
	}
	return s, xs, sub, fps
}

func (s *xraySupervisor) testSlots(t *testing.T) []xray.DialerSlot {
	t.Helper()
	entries, err := s.xrayStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slots, err := s.resolveDialerSlots(context.Background(), entries)
	if err != nil {
		t.Fatal(err)
	}
	s.loadedSlots = slots
	return slots
}

func TestResolvePoolStrategies(t *testing.T) {
	ctx := context.Background()
	s, xs, sub, fps := newStrategyRig(t, 4)

	// Default: auto, run stable until the classifier says otherwise.
	slots := s.testSlots(t)
	if len(slots) != 1 || slots[0].Strategy != xray.StrategyStable || !slots[0].Auto || slots[0].PoolKey != "xraysub:pool" {
		t.Fatalf("default slot = %+v", slots)
	}

	// Stable parks; agile brings the parked node back and forgets it.
	s.parker.nodes[fps[0]] = &parkState{parkedUntil: time.Now().Add(time.Hour)}
	if slots = s.testSlots(t); len(slots[0].Members) != 3 {
		t.Errorf("stable pool kept a parked node: %d members", len(slots[0].Members))
	}
	if err := xs.SetSubStrategy(ctx, sub.ID, "agile"); err != nil {
		t.Fatal(err)
	}
	slots = s.testSlots(t)
	if !slots[0].Agile() || slots[0].Auto || len(slots[0].Members) != 4 || len(s.parker.parked()) != 0 {
		t.Errorf("agile slot = %+v, parked %v", slots[0], s.parker.parked())
	}
	// The agile pick drives the balancer wiring.
	s.agile.picks["xraysub:pool"] = agilePick{active: []string{fps[1], fps[2]}, spare: fps[3]}
	slots = s.testSlots(t)
	if !reflect.DeepEqual(slots[0].Preferred, []string{fps[1], fps[2]}) || slots[0].Expected != 2 || slots[0].Fallback != fps[3] {
		t.Errorf("agile wiring = pref %v expected %d fallback %q", slots[0].Preferred, slots[0].Expected, slots[0].Fallback)
	}

	// Manual with no pin runs stable; with pins, the pool is the pins.
	if err := xs.SetSubStrategy(ctx, sub.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if slots = s.testSlots(t); slots[0].Strategy != xray.StrategyStable || slots[0].Auto {
		t.Errorf("pinless manual = %+v", slots[0])
	}
	if err := xs.SetNodePinned(ctx, sub.ID, fps[2], true); err != nil {
		t.Fatal(err)
	}
	slots = s.testSlots(t)
	if !slots[0].Manual() || len(slots[0].Members) != 1 || slots[0].Members[0].Key != fps[2] || !reflect.DeepEqual(slots[0].Pinned, []string{fps[2]}) {
		t.Errorf("manual slot = %+v", slots[0])
	}

	// Explicit stable is not auto.
	if err := xs.SetSubStrategy(ctx, sub.ID, "stable"); err != nil {
		t.Fatal(err)
	}
	if slots = s.testSlots(t); slots[0].Strategy != xray.StrategyStable || slots[0].Auto {
		t.Errorf("stable slot = %+v", slots[0])
	}
	if err := xs.SetSubStrategy(ctx, sub.ID, "sideways"); err == nil {
		t.Error("accepted an unknown strategy")
	}
}

func TestPinsSurviveCapAndDisable(t *testing.T) {
	ctx := context.Background()
	_, xs, sub, fps := newStrategyRig(t, 4)
	sub.NodeCap = 2
	if err := xs.UpdateSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	// Pin the last node: active past the cap.
	if err := xs.SetNodePinned(ctx, sub.ID, fps[3], true); err != nil {
		t.Fatal(err)
	}
	active := func() map[string]bool {
		nodes, _ := xs.ListNodes(ctx, sub.ID)
		out := map[string]bool{}
		for _, n := range nodes {
			if n.Active {
				out[n.Fingerprint] = true
			}
		}
		return out
	}
	if a := active(); !a[fps[3]] || len(a) != 3 {
		t.Errorf("active with pin past cap = %v", a)
	}
	// Disabling unpins; re-enabling doesn't re-pin.
	if err := xs.SetNodeDisabled(ctx, sub.ID, fps[3], true); err != nil {
		t.Fatal(err)
	}
	if pins, _ := xs.PinnedFingerprints(ctx, sub.ID); len(pins) != 0 {
		t.Errorf("disabled node still pinned: %v", pins)
	}
	// Pinning a disabled node enables it.
	if err := xs.SetNodePinned(ctx, sub.ID, fps[3], true); err != nil {
		t.Fatal(err)
	}
	if dis, _ := xs.DisabledFingerprints(ctx, sub.ID); dis[fps[3]] {
		t.Error("pinned node still disabled")
	}
	// Un-disabling a pinned node keeps the pin.
	if err := xs.SetNodePinned(ctx, sub.ID, fps[0], true); err != nil {
		t.Fatal(err)
	}
	if err := xs.SetNodeDisabled(ctx, sub.ID, fps[0], false); err != nil {
		t.Fatal(err)
	}
	if pins, _ := xs.PinnedFingerprints(ctx, sub.ID); !pins[fps[0]] || !pins[fps[3]] {
		t.Errorf("pins = %v", pins)
	}
	if err := xs.ClearPins(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if pins, _ := xs.PinnedFingerprints(ctx, sub.ID); len(pins) != 0 {
		t.Errorf("pins after clear = %v", pins)
	}
}

// Import writes the strategy through AddSub; it must land.
func TestAddSubKeepsStrategy(t *testing.T) {
	_, xs, _, _ := newStrategyRig(t, 0)
	sub, err := xs.AddSub(context.Background(), xray.Subscription{Name: "other", URL: "https://example.invalid/2", Enabled: true, Strategy: xray.StrategyAgile})
	if err != nil {
		t.Fatal(err)
	}
	got, err := xs.GetSub(context.Background(), sub.ID)
	if err != nil || got.EffectiveStrategy() != xray.StrategyAgile {
		t.Errorf("strategy = %q (%v)", got.Strategy, err)
	}
}
