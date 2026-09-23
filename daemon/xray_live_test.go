package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ehsan/em-wall/core/xray"
)

func genLive(t *testing.T, entries []xray.Config, opt xray.GenerateOptions) (liveConfig, []byte) {
	t.Helper()
	raw, err := xray.Generate(entries, opt)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	lc, err := parseLiveConfig(raw)
	if err != nil {
		t.Fatalf("parseLiveConfig: %v", err)
	}
	return lc, raw
}

func entry(name string, port int, host string) xray.Config {
	return xray.Config{
		Name: name, SocksPort: port, Enabled: true,
		Outbound: `{"protocol":"vmess","settings":{"vnext":[{"address":"` + host + `","port":443,"users":[{"id":"x"}]}]}}`,
	}
}

// Everything a user does day to day must apply without a restart, and must
// touch only the objects that actually changed.
func TestPlanLive(t *testing.T) {
	base := []xray.Config{entry("a", 11800, "a.example"), entry("b", 11801, "b.example")}
	old, _ := genLive(t, base, xray.GenerateOptions{})

	t.Run("identical", func(t *testing.T) {
		now, _ := genLive(t, base, xray.GenerateOptions{})
		if p := planLive(old, now); !p.empty() {
			t.Fatalf("plan = %+v, want empty", p)
		}
	})

	t.Run("entry edited", func(t *testing.T) {
		now, _ := genLive(t, []xray.Config{entry("a", 11800, "a2.example"), base[1]}, xray.GenerateOptions{})
		p := planLive(old, now)
		if p.restart || p.routing {
			t.Fatalf("plan = %+v, want outbound-only", p)
		}
		want := []string{xray.OutboundTag("a")}
		if !reflect.DeepEqual(p.rmOut, want) || !reflect.DeepEqual(p.addOut, want) || len(p.rmIn)+len(p.addIn) != 0 {
			t.Fatalf("plan = %+v, want only out-a replaced", p)
		}
	})

	t.Run("entry added", func(t *testing.T) {
		now, _ := genLive(t, append(append([]xray.Config{}, base...), entry("c", 11802, "c.example")), xray.GenerateOptions{})
		p := planLive(old, now)
		if p.restart || !p.routing || len(p.rmOut)+len(p.rmIn) != 0 {
			t.Fatalf("plan = %+v, want add + routing", p)
		}
		if !reflect.DeepEqual(p.addOut, []string{xray.OutboundTag("c")}) || !reflect.DeepEqual(p.addIn, []string{xray.InboundTag("c")}) {
			t.Fatalf("plan = %+v, want c added", p)
		}
	})

	t.Run("entry sorted first is still live", func(t *testing.T) {
		// Before blackhole-first, a new alphabetically-first entry moved
		// xray's default outbound and forced a restart.
		now, _ := genLive(t, append([]xray.Config{entry("0first", 11803, "z.example")}, base...), xray.GenerateOptions{})
		if p := planLive(old, now); p.restart {
			t.Fatalf("adding an entry forced a restart")
		}
	})

	t.Run("entry removed", func(t *testing.T) {
		now, _ := genLive(t, base[:1], xray.GenerateOptions{})
		p := planLive(old, now)
		if p.restart || len(p.addOut)+len(p.addIn) != 0 {
			t.Fatalf("plan = %+v, want removals only", p)
		}
	})

	t.Run("master added", func(t *testing.T) {
		m := entry("b", 11801, "b.example")
		m.Dialer = "xraysub:s"
		slots := []xray.DialerSlot{{Master: "b", Index: 0, Members: []xray.DialerMember{
			{Key: "n1", Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[]}}`)},
		}}}
		now, _ := genLive(t, []xray.Config{base[0], m}, xray.GenerateOptions{DialerSlots: slots})
		p := planLive(old, now)
		if p.restart {
			t.Fatalf("first master forced a restart, plan = %+v", p)
		}
		// Pool churn after that is live.
		slots2 := []xray.DialerSlot{{Master: "b", Index: 0, Members: []xray.DialerMember{
			{Key: "n2", Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[]}}`)},
		}}}
		later, _ := genLive(t, []xray.Config{base[0], m}, xray.GenerateOptions{DialerSlots: slots2})
		p = planLive(now, later)
		if p.restart || len(p.addOut) != 1 || len(p.rmOut) != 1 {
			t.Fatalf("pool churn plan = %+v, want one member swapped live", p)
		}
	})

	t.Run("user routing rules", func(t *testing.T) {
		now, _ := genLive(t, base, xray.GenerateOptions{RoutingRules: `[{"type":"field","domain":["x.com"],"outboundTag":"block"}]`})
		p := planLive(old, now)
		if p.restart || !p.routing || len(p.addOut)+len(p.rmOut) != 0 {
			t.Fatalf("plan = %+v, want routing-only", p)
		}
	})
}

// Formatting and key order must never read as a change.
func TestParseLiveConfigCanonical(t *testing.T) {
	a := []byte(`{"log":{"loglevel":"warning"},"inbounds":[{"tag":"i","port":1}],"outbounds":[{"tag":"o","protocol":"freedom"}],"routing":{"rules":[]}}`)
	b := []byte(`{ "routing": {"rules": []}, "outbounds": [{"protocol":"freedom", "tag":"o"}], "inbounds":[{"port":1,"tag":"i"}], "log":{"loglevel":"warning"}}`)
	la, err := parseLiveConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := parseLiveConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if p := planLive(la, lb); !p.empty() {
		t.Fatalf("plan = %+v, want empty", p)
	}
	if _, err := parseLiveConfig([]byte(`{"outbounds":[{"tag":"o"},{"tag":"o"}]}`)); err == nil {
		t.Fatalf("duplicate tags must be rejected")
	}
}
