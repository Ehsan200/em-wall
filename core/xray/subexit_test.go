package xray

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubInterfaceHelpers(t *testing.T) {
	if got := ParseSubName("xraysub: Foo "); got != "foo" {
		t.Errorf("ParseSubName = %q", got)
	}
	if ParseSubName("xrayset:foo") != "" || ParseSubName("xray:foo") != "" {
		t.Error("ParseSubName accepted a non-sub interface")
	}
	if got := CanonicalizeInterface("xraysub:MySub"); got != "xraysub:mysub" {
		t.Errorf("CanonicalizeInterface = %q", got)
	}
	if !IsIndirectInterface("xraysub:a") || !IsIndirectInterface("xrayset:a") || IsIndirectInterface("xray:a") {
		t.Error("IsIndirectInterface wrong")
	}
	if !IsInternalProxyName(SubProxyName(7)) || !IsInternalProxyName(InternalProxyName("x")) || IsInternalProxyName("user") {
		t.Error("IsInternalProxyName wrong")
	}
	exp := map[string]string{"nj": "xray:a,b", "xraysub:pool": "proxy:_xsub_3"}
	if got := ExpandSetInterface("xraysub:pool", exp); got != "proxy:_xsub_3" {
		t.Errorf("expand sub = %q", got)
	}
	if got := ExpandSetInterface("xraysub:gone", exp); got != "xraysub:gone" {
		t.Errorf("missing sub must stay verbatim (fail closed), got %q", got)
	}
	if got := ExpandSetInterface("xrayset:nj", exp); got != "xray:a,b" {
		t.Errorf("expand set = %q", got)
	}
}

func TestSubExpansionsOnlyEnabled(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	on, err := st.AddSub(ctx, Subscription{Name: "on", URL: "https://example.com/a", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	off, err := st.AddSub(ctx, Subscription{Name: "off", URL: "https://example.com/b", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubEnabled(ctx, off.ID, false); err != nil {
		t.Fatal(err)
	}
	exp, err := st.Expansions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := exp["xraysub:on"]; got != "proxy:"+SubProxyName(on.ID) {
		t.Errorf("xraysub:on → %q", got)
	}
	if _, ok := exp["xraysub:off"]; ok {
		t.Error("disabled subscription must not expand")
	}
}

func TestSlotExitOnly(t *testing.T) {
	s := DialerSlot{Master: ExitOwner("Pool"), ExitOnly: true, Exits: []SlotExit{{Sub: "pool", SubID: 4}}}
	if len(s.SlotMasters()) != 0 {
		t.Errorf("exit-only slot has masters %v", s.SlotMasters())
	}
	if got := s.ProxyNames(); len(got) != 1 || got[0] != SubProxyName(4) {
		t.Errorf("ProxyNames = %v", got)
	}
	m := DialerSlot{Master: "m", Aliases: []string{"n"}, Exits: []SlotExit{{Sub: "p", SubID: 1}}}
	if got := strings.Join(m.ProxyNames(), ","); got != "_xray_m,_xray_n,_xsub_1" {
		t.Errorf("ProxyNames = %s", got)
	}
}

func TestGenerate_ExitOnlySlotAndProbes(t *testing.T) {
	entry := Config{Name: "e", Enabled: true, SocksPort: 11800,
		Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"e.example","port":443,"users":[{"id":"y"}]}]}}`}
	node := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"node.example","port":443,"users":[{"id":"x"}]}]}}`)
	raw, err := Generate([]Config{entry}, GenerateOptions{
		DialerSlots: []DialerSlot{{
			Master: ExitOwner("pool"), ExitOnly: true, Index: 0,
			Members: []DialerMember{{Key: "fp1", Outbound: node}, {Key: "fp2", Outbound: node}},
			Exits:   []SlotExit{{Sub: "pool", SubID: 9}},
		}},
		ExitProbes: []ExitProbe{{SubID: 9, Key: "fp1", Outbound: node}, {SubID: 9, Key: "bad", Outbound: json.RawMessage(`nope`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Inbounds []struct {
			Tag      string         `json:"tag"`
			Settings map[string]any `json:"settings"`
		} `json:"inbounds"`
		Outbounds []map[string]any `json:"outbounds"`
		Routing   struct {
			Rules []map[string]any `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	for _, ob := range cfg.Outbounds {
		tag, _ := ob["tag"].(string)
		tags[tag] = true
		if strings.HasPrefix(tag, "dialer-") {
			t.Errorf("exit-only slot emitted a dialer outbound %q", tag)
		}
	}
	for _, want := range []string{SlotMemberTag(0, "fp1"), SlotMemberTag(0, "fp2"), ExitProbeTag(9, "fp1")} {
		if !tags[want] {
			t.Errorf("missing outbound %q", want)
		}
	}
	if tags[ExitProbeTag(9, "bad")] {
		t.Error("unparseable probe node was emitted")
	}
	if strings.HasPrefix(ExitProbeTag(9, "fp1"), ObservatorySelectorPrefix) {
		t.Error("exit probe tag would be pinged by the observatory")
	}
	var slotIn, probeRoute bool
	for _, in := range cfg.Inbounds {
		if in.Tag == SlotInboundTag(0) {
			slotIn = true
		}
	}
	for _, r := range cfg.Routing.Rules {
		if r["outboundTag"] == ExitProbeTag(9, "fp1") {
			probeRoute = true
		}
	}
	if !slotIn {
		t.Error("exit-only slot has no inbound")
	}
	if !probeRoute {
		t.Error("exit probe not routed from the probe inbound")
	}
}

func TestGenerate_ExitProbesWithoutSlots(t *testing.T) {
	entry := Config{Name: "e", Enabled: true, SocksPort: 11800,
		Outbound: `{"protocol":"freedom"}`}
	raw, err := Generate([]Config{entry}, GenerateOptions{
		ExitProbes: []ExitProbe{{SubID: 1, Key: "k", Outbound: json.RawMessage(`{"protocol":"freedom"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"`+ProbeTag+`"`) {
		t.Error("probe inbound missing when only exit probes exist")
	}
}
