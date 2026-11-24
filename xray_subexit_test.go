package main

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

func exitObs(fp, ip, cc string) xray.SubExit {
	return xray.SubExit{Fingerprint: fp, IP: ip, Country: cc}
}

func TestSubExitVerdict(t *testing.T) {
	cases := []struct {
		name       string
		fps        []string
		obs        []xray.SubExit
		ack        string
		verdict    string
		needsAck   bool
		unmeasured int
		rotating   int
	}{
		{name: "nothing measured", fps: []string{"a", "b"}, verdict: "unverified", unmeasured: 2},
		{name: "one shared exit", fps: []string{"a", "b"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", "DE"), exitObs("b", "1.1.1.1", "DE")}, verdict: "single"},
		{name: "one exit, one node unmeasured", fps: []string{"a", "b", "c"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", "DE"), exitObs("b", "1.1.1.1", "DE")}, verdict: "single", unmeasured: 1},
		{name: "two exits", fps: []string{"a", "b"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", "DE"), exitObs("b", "2.2.2.2", "NL")}, verdict: "multi", needsAck: true},
		{name: "two exits accepted", fps: []string{"a", "b"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", "DE"), exitObs("b", "2.2.2.2", "NL")}, ack: "1.1.1.1,2.2.2.2", verdict: "multi"},
		{name: "new exit beyond accepted", fps: []string{"a", "b", "c"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", ""), exitObs("b", "2.2.2.2", ""), exitObs("c", "3.3.3.3", "")},
			ack: "1.1.1.1,2.2.2.2", verdict: "multi", needsAck: true},
		{name: "single rotating node", fps: []string{"a"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", ""), exitObs("a", "1.1.1.2", "")}, verdict: "multi", needsAck: true, rotating: 1},
		{name: "uncounted node ignored", fps: []string{"a"},
			obs: []xray.SubExit{exitObs("a", "1.1.1.1", ""), exitObs("z", "9.9.9.9", "")}, verdict: "single"},
	}
	for _, c := range cases {
		v := subExitVerdict(c.fps, c.obs, xray.SplitExitIPs(c.ack))
		if v.verdict != c.verdict || v.needsAck != c.needsAck || v.unmeasured != c.unmeasured || len(v.rotating) != c.rotating {
			t.Errorf("%s: got verdict=%s needsAck=%v unmeasured=%d rotating=%d", c.name, v.verdict, v.needsAck, v.unmeasured, len(v.rotating))
		}
	}
}

func TestParseTrace(t *testing.T) {
	ip, cc, ok := parseTrace(strings.NewReader("fl=1\nh=1.1.1.1\nip=203.0.113.7\nts=1\nloc=de\n"))
	if !ok || ip != "203.0.113.7" || cc != "DE" {
		t.Errorf("v4 trace = %q %q %v", ip, cc, ok)
	}
	if _, _, ok := parseTrace(strings.NewReader("ip=2001:db8::1\nloc=DE\n")); ok {
		t.Error("an IPv6 exit must not be recorded")
	}
	if _, _, ok := parseTrace(strings.NewReader("garbage")); ok {
		t.Error("garbage accepted")
	}
}

func TestRuleExitNames(t *testing.T) {
	got := ruleExitNames([]rules.Rule{
		{Pattern: "a.com", Action: rules.ActionRoute, Interface: "xraysub:Pool", Enabled: true},
		{Pattern: "b.com", Action: rules.ActionRoute, Interface: "xraysub:off", Enabled: false},
		{Pattern: "c.com", Action: rules.ActionRoute, Interface: "xray:x", Enabled: true},
	})
	if len(got) != 1 || !got["pool"] {
		t.Errorf("ruleExitNames = %v", got)
	}
}

func TestExitWatchReconcilesOnlyOnChange(t *testing.T) {
	n := 0
	w := &exitWatch{reconcile: func(context.Context) error { n++; return nil }}
	ctx := context.Background()
	on := []rules.Rule{{Pattern: "a.com", Action: rules.ActionRoute, Interface: "xraysub:p", Enabled: true}}
	w.onReload(ctx, on)
	w.onReload(ctx, on)
	w.onReload(ctx, append(on, rules.Rule{Pattern: "b.com", Action: rules.ActionBlock, Enabled: true}))
	if n != 1 {
		t.Errorf("reconciled %d times for one change", n)
	}
	w.onReload(ctx, nil)
	if n != 2 {
		t.Errorf("removing the last exit rule must reconcile (n=%d)", n)
	}
}

// subExitRig: rule + xray stores on one DB, a subscription "pool" with
// nodes n0..n2, and deps wired with a prober over them.
func subExitRig(t *testing.T) (*handlerDeps, xray.Subscription, []string) {
	t.Helper()
	s, xs, sub, fps := newStrategyRig(t, 3)
	rs, err := rules.Open(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	ps, err := proxy.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	s.proxyStore = ps
	d := &handlerDeps{store: rs, xrayStore: xs, proxyStore: ps, xraySup: s,
		subExits: newSubExitProber(xs, rs, nil, log.New(io.Discard, "", 0))}
	return d, sub, fps
}

func TestCheckExitAck(t *testing.T) {
	ctx := context.Background()
	d, sub, fps := subExitRig(t)
	now := time.Now().UTC()

	// Unverified: nothing to confirm.
	if _, err := d.validateRuleIface(ctx, "xraysub:Pool", exitAckAsk); err != nil {
		t.Fatalf("unverified subscription refused: %v", err)
	}
	// One shared exit: no confirmation needed.
	for _, fp := range fps {
		if err := d.xrayStore.RecordSubExit(ctx, sub.ID, fp, "1.1.1.1", "DE", now); err != nil {
			t.Fatal(err)
		}
	}
	if iface, err := d.validateRuleIface(ctx, "xraysub:Pool", exitAckAsk); err != nil || iface != "xraysub:pool" {
		t.Fatalf("single exit: %q %v", iface, err)
	}
	// A second exit: refused until confirmed.
	if err := d.xrayStore.RecordSubExit(ctx, sub.ID, fps[1], "2.2.2.2", "NL", now); err != nil {
		t.Fatal(err)
	}
	_, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckAsk)
	if err == nil || !strings.HasPrefix(err.Error(), "multi-exit:") {
		t.Fatalf("multi exit without confirm: %v", err)
	}
	// An unchanged binding is never blocked — and accepts nothing.
	if _, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckSkip); err != nil {
		t.Fatalf("skip refused: %v", err)
	}
	if _, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckAsk); err == nil {
		t.Fatal("skip must not accept the exits")
	}
	if _, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckGiven); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if _, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckAsk); err != nil {
		t.Fatalf("accepted exits asked again: %v", err)
	}
	// Drift: a new exit asks again, and shows as a status warning once a
	// rule uses the subscription.
	if err := d.xrayStore.RecordSubExit(ctx, sub.ID, fps[2], "3.3.3.3", "US", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.validateRuleIface(ctx, "xraysub:pool", exitAckAsk); err == nil {
		t.Fatal("new exit did not ask again")
	}
	if w := d.subExits.Warnings(ctx); len(w) != 0 {
		t.Errorf("warning for an unused subscription: %v", w)
	}
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "a.com", Action: rules.ActionRoute, Interface: "xraysub:pool", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if w := d.subExits.Warnings(ctx); len(w) != 1 || w[0] != "pool" {
		t.Errorf("Warnings = %v", w)
	}
	// Unknown and multi-name refs.
	if _, err := d.validateRuleIface(ctx, "xraysub:nope", exitAckAsk); err == nil {
		t.Error("unknown subscription accepted")
	}
	if _, err := d.validateRuleIface(ctx, "xraysub:pool,other", exitAckAsk); err == nil {
		t.Error("multi-subscription ref accepted")
	}
}

func TestCountedNodesManualPins(t *testing.T) {
	ctx := context.Background()
	d, sub, fps := subExitRig(t)
	if err := d.xrayStore.SetSubStrategy(ctx, sub.ID, xray.StrategyManual); err != nil {
		t.Fatal(err)
	}
	sub, _ = d.xrayStore.GetSub(ctx, sub.ID)
	if got, pinned := d.subExits.countedNodes(ctx, sub); len(got) != 3 || pinned {
		t.Errorf("manual without pins counts every active node: %v %v", got, pinned)
	}
	if err := d.xrayStore.SetNodePinned(ctx, sub.ID, fps[0], true); err != nil {
		t.Fatal(err)
	}
	if got, pinned := d.subExits.countedNodes(ctx, sub); len(got) != 1 || got[0] != fps[0] || !pinned {
		t.Errorf("manual with a pin counts only it: %v %v", got, pinned)
	}
}

func TestResolveSlotsWithSubExits(t *testing.T) {
	ctx := context.Background()
	s, xs, sub, _ := newStrategyRig(t, 3)
	exits := map[string]bool{"pool": true}
	s.ruleExits = func(context.Context) map[string]bool { return exits }

	// Master "m" dials exactly xraysub:pool: the exit shares its slot.
	slots := s.testSlots(t)
	if len(slots) != 1 || slots[0].ExitOnly || len(slots[0].Exits) != 1 || slots[0].Exits[0].SubID != sub.ID {
		t.Fatalf("shared slot = %+v", slots)
	}

	// Master moves to a mixed pool: the exit gets its own slot after it.
	m, err := xs.GetByName(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xs.Add(ctx, xray.Config{Name: "other", Enabled: true,
		Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"o.example","port":443,"users":[{"id":"z"}]}]}}`}); err != nil {
		t.Fatal(err)
	}
	m.Dialer = "xraysub:pool,xray:other"
	if err := xs.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	slots = s.testSlots(t)
	if len(slots) != 2 {
		t.Fatalf("want master + exit-only slot, got %+v", slots)
	}
	ex := slots[1]
	if !ex.ExitOnly || ex.Index != 1 || ex.Master != xray.ExitOwner("pool") || len(ex.Members) != 3 || len(ex.SlotMasters()) != 0 {
		t.Errorf("exit-only slot = %+v", ex)
	}
	if len(slots[0].Exits) != 0 {
		t.Errorf("mixed master pool must not carry the exit: %+v", slots[0].Exits)
	}

	// Disabled subscription: no exit slot.
	if err := xs.SetSubEnabled(ctx, sub.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, sl := range s.testSlots(t) {
		if len(sl.Exits) > 0 {
			t.Errorf("disabled subscription still has an exit slot: %+v", sl)
		}
	}
}

func TestSyncProxiesSubExitRows(t *testing.T) {
	ctx := context.Background()
	d, sub, _ := subExitRig(t)
	s := d.xraySup
	slots := []xray.DialerSlot{{Master: xray.ExitOwner("pool"), ExitOnly: true, Index: 3,
		Exits: []xray.SlotExit{{Sub: "pool", SubID: sub.ID}}}}
	if err := s.syncProxies(ctx, nil, slots); err != nil {
		t.Fatal(err)
	}
	p, err := d.proxyStore.GetByName(ctx, xray.SubProxyName(sub.ID))
	if err != nil || p.Port != xray.SlotPort(3) || p.Host != "127.0.0.1" {
		t.Fatalf("exit row = %+v, %v", p, err)
	}
	// Slot gone → row gone → the rule fails closed.
	if err := s.syncProxies(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.proxyStore.GetByName(ctx, xray.SubProxyName(sub.ID)); err == nil {
		t.Error("exit row survived its slot")
	}
}

func TestDisplayUpstreamSubExit(t *testing.T) {
	publishSubExitLabels([]xray.DialerSlot{{Exits: []xray.SlotExit{{Sub: "pool", SubID: 5}}}})
	if got := displayUpstream(xray.SubProxyName(5)); got != "sub:pool" {
		t.Errorf("displayUpstream = %q", got)
	}
	if got := displayUpstream(xray.InternalProxyName("nj")); got != "nj" {
		t.Errorf("displayUpstream = %q", got)
	}
}
