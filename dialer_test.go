package xray

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseDialer(t *testing.T) {
	cases := []struct {
		in   string
		want []DialerRef
		err  bool
	}{
		{"", nil, false},
		{"  ", nil, false},
		{"xray:a", []DialerRef{{DialerKindXray, "a"}}, false},
		{"xray:A, xraysub:Sub1 ,proxy:p", []DialerRef{
			{DialerKindXray, "a"}, {DialerKindXraysub, "sub1"}, {DialerKindProxy, "p"},
		}, false},
		{"bogus:a", nil, true},
		{"noprefix", nil, true},
		{"xray:", nil, true},
		{"xray:bad name!", nil, true},
	}
	for _, c := range cases {
		got, err := ParseDialer(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseDialer(%q): want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDialer(%q): unexpected error %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("ParseDialer(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseDialer(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestStore_DialerCanonicalizedAndCycleRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Self-reference by xray:<self> is rejected.
	_, err := s.Add(ctx, Config{
		Name:     "loopy",
		Outbound: `{"protocol":"freedom"}`,
		Enabled:  true,
		Dialer:   "xray:loopy",
	})
	if !errors.Is(err, ErrDialerCycle) {
		t.Errorf("self-dialer: err = %v, want ErrDialerCycle", err)
	}

	// A valid dialer is stored in canonical (normalized) form.
	c, err := s.Add(ctx, Config{
		Name:     "master",
		Outbound: `{"protocol":"freedom"}`,
		Enabled:  true,
		Dialer:   " xraysub:Fast , proxy:P ",
	})
	if err != nil {
		t.Fatalf("Add master: %v", err)
	}
	if c.Dialer != "xraysub:fast,proxy:p" {
		t.Errorf("stored Dialer = %q, want canonical %q", c.Dialer, "xraysub:fast,proxy:p")
	}

	// Bad dialer syntax on update is rejected.
	c.Dialer = "wat:x"
	if err := s.Update(ctx, c); !errors.Is(err, ErrInvalidDialer) {
		t.Errorf("bad dialer update: err = %v, want ErrInvalidDialer", err)
	}
}

func TestDetectDialerCycle(t *testing.T) {
	// a → b → c, no cycle.
	entries := []Config{
		{ID: 1, Name: "a", Dialer: "xray:b"},
		{ID: 2, Name: "b", Dialer: "xray:c"},
		{ID: 3, Name: "c", Dialer: ""},
	}
	if DetectDialerCycle(entries, 3, "c", "") {
		t.Error("a→b→c should have no cycle")
	}
	// Update c to dial a → a→b→c→a cycle.
	if !DetectDialerCycle(entries, 3, "c", "xray:a") {
		t.Error("c→a should close a cycle a→b→c→a")
	}
	// Self-loop.
	if !DetectDialerCycle(entries, 1, "a", "xray:a") {
		t.Error("a→a self-loop should be a cycle")
	}
	// Add (selfID 0) a new master referencing existing leaf — no cycle.
	if DetectDialerCycle(entries, 0, "d", "xray:c,xraysub:s1") {
		t.Error("new master d→c (+sub leaf) should have no cycle")
	}
	// xraysub/proxy refs are leaves — can't form a cycle even if named like a.
	if DetectDialerCycle(entries, 3, "c", "xraysub:a,proxy:a") {
		t.Error("sub/proxy refs must not be treated as entry edges")
	}
}

func TestGenerate_DialerSlot(t *testing.T) {
	master := Config{
		Name:     "m",
		Enabled:  true,
		Dialer:   "xraysub:sub1",
		Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"master.example","port":443,"users":[{"id":"y"}]}]}}`,
	}
	member := DialerMember{
		Key:      "fp1",
		Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"node.example","port":443,"users":[{"id":"x"}]}]}}`),
	}
	raw, err := Generate([]Config{master}, GenerateOptions{
		DialerSlots: []DialerSlot{{Master: "m", Index: 0, Members: []DialerMember{member}}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var cfg struct {
		Inbounds []struct {
			Tag  string `json:"tag"`
			Port int    `json:"port"`
		} `json:"inbounds"`
		Outbounds        []map[string]any `json:"outbounds"`
		BurstObservatory struct {
			SubjectSelector []string `json:"subjectSelector"`
			PingConfig      struct {
				Destination string `json:"destination"`
				Interval    string `json:"interval"`
				Sampling    int    `json:"sampling"`
				Timeout     string `json:"timeout"`
			} `json:"pingConfig"`
		} `json:"burstObservatory"`
		Routing struct {
			Balancers []struct {
				Tag      string   `json:"tag"`
				Selector []string `json:"selector"`
				Strategy struct {
					Type string `json:"type"`
				} `json:"strategy"`
			} `json:"balancers"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal generated config: %v\n%s", err, raw)
	}

	// Slot inbound present on the expected port.
	var haveSlotIn bool
	for _, in := range cfg.Inbounds {
		if in.Tag == SlotInboundTag(0) {
			haveSlotIn = true
			if in.Port != SlotPort(0) {
				t.Errorf("slot inbound port = %d, want %d", in.Port, SlotPort(0))
			}
		}
	}
	if !haveSlotIn {
		t.Errorf("missing slot inbound tag %q", SlotInboundTag(0))
	}

	// Outbounds: master carries sockopt.dialerProxy; dialer socks + member exist.
	var haveDialer, haveMember, sockOK bool
	for _, ob := range cfg.Outbounds {
		switch ob["tag"] {
		case OutboundTag("m"):
			ss, _ := ob["streamSettings"].(map[string]any)
			sock, _ := ss["sockopt"].(map[string]any)
			if sock["dialerProxy"] == DialerOutboundTag("m") {
				sockOK = true
			}
		case DialerOutboundTag("m"):
			haveDialer = true
		case SlotMemberTag(0, "fp1"):
			haveMember = true
		}
	}
	if !sockOK {
		t.Errorf("master outbound missing sockopt.dialerProxy = %q", DialerOutboundTag("m"))
	}
	if !haveDialer {
		t.Errorf("missing dialer socks outbound %q", DialerOutboundTag("m"))
	}
	if !haveMember {
		t.Errorf("missing slot member outbound %q", SlotMemberTag(0, "fp1"))
	}

	// Balancer with the slot's prefix selector + leastLoad.
	var haveBal bool
	for _, b := range cfg.Routing.Balancers {
		if b.Tag == SlotBalancerTag(0) {
			haveBal = true
			if len(b.Selector) != 1 || b.Selector[0] != SlotOutboundPrefix(0) {
				t.Errorf("balancer selector = %v, want [%q]", b.Selector, SlotOutboundPrefix(0))
			}
			if b.Strategy.Type != "leastLoad" {
				t.Errorf("balancer strategy = %q, want leastLoad", b.Strategy.Type)
			}
		}
	}
	if !haveBal {
		t.Errorf("missing balancer %q", SlotBalancerTag(0))
	}

	// Shared burst observatory with the default ping config + prefix selector.
	if cfg.BurstObservatory.PingConfig.Destination != DefaultProbeURL {
		t.Errorf("burst pingConfig destination = %q, want %q", cfg.BurstObservatory.PingConfig.Destination, DefaultProbeURL)
	}
	if cfg.BurstObservatory.PingConfig.Interval != DefaultProbeInterval {
		t.Errorf("burst pingConfig interval = %q, want %q", cfg.BurstObservatory.PingConfig.Interval, DefaultProbeInterval)
	}
	if cfg.BurstObservatory.PingConfig.Sampling != DefaultProbeSampling {
		t.Errorf("burst pingConfig sampling = %d, want %d", cfg.BurstObservatory.PingConfig.Sampling, DefaultProbeSampling)
	}
	if len(cfg.BurstObservatory.SubjectSelector) != 1 || cfg.BurstObservatory.SubjectSelector[0] != ObservatorySelectorPrefix {
		t.Errorf("burst observatory selector = %v, want [%q]", cfg.BurstObservatory.SubjectSelector, ObservatorySelectorPrefix)
	}
}

func TestGenerate_ApiBlockAlwaysOn(t *testing.T) {
	member := DialerMember{Key: "fp1", Outbound: json.RawMessage(`{"protocol":"freedom"}`)}
	withSlots, err := Generate([]Config{{Name: "m", Enabled: true, Dialer: "xray:x", Outbound: `{"protocol":"freedom"}`}},
		GenerateOptions{DialerSlots: []DialerSlot{{Master: "m", Index: 0, Members: []DialerMember{member}}}})
	if err != nil {
		t.Fatalf("Generate with slots: %v", err)
	}
	var cfg struct {
		API      json.RawMessage `json:"api"`
		Stats    json.RawMessage `json:"stats"`
		Inbounds []struct {
			Tag      string `json:"tag"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(withSlots, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.API) == 0 || len(cfg.Stats) == 0 {
		t.Errorf("api/stats block missing when slots present")
	}
	var haveAPIInbound bool
	for _, in := range cfg.Inbounds {
		if in.Tag == ApiTag {
			haveAPIInbound = true
			if in.Port != ApiPort || in.Protocol != "dokodemo-door" {
				t.Errorf("api inbound = port %d proto %q, want %d dokodemo-door", in.Port, in.Protocol, ApiPort)
			}
		}
	}
	if !haveAPIInbound {
		t.Errorf("api inbound missing when slots present")
	}

	// No slots → the api block is still there: every config change is
	// applied to the running process through it.
	noSlots, _ := Generate([]Config{{Name: "a", Enabled: true, Outbound: `{"protocol":"freedom"}`}}, GenerateOptions{})
	var plain struct {
		API json.RawMessage `json:"api"`
	}
	_ = json.Unmarshal(noSlots, &plain)
	if len(plain.API) == 0 {
		t.Errorf("api block missing with no slots")
	}
}

func TestGenerate_NoSlotsNoBalancers(t *testing.T) {
	raw, err := Generate([]Config{{Name: "a", Enabled: true, Outbound: `{"protocol":"freedom"}`}}, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		BurstObservatory json.RawMessage `json:"burstObservatory"`
		Routing          struct {
			Balancers json.RawMessage `json:"balancers"`
		} `json:"routing"`
	}
	_ = json.Unmarshal(raw, &cfg)
	// The observatory is always present (so the first master applies
	// live); with no slots it simply has nothing to probe.
	if len(cfg.BurstObservatory) == 0 {
		t.Errorf("burst observatory missing with no slots")
	}
	if len(cfg.Routing.Balancers) != 0 {
		t.Errorf("balancers emitted with no slots: %s", cfg.Routing.Balancers)
	}
}

// Masters sharing one slot each keep their own dialer outbound (so their
// sockopt wiring is unchanged) but the member outbounds — what the
// observatory probes — exist once.
func TestGenerate_SharedSlotAliases(t *testing.T) {
	mk := func(name string) Config {
		return Config{
			Name: name, Enabled: true, Dialer: "xraysub:sub1",
			Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"m.example","port":443,"users":[{"id":"y"}]}]}}`,
		}
	}
	member := DialerMember{
		Key:      "fp1",
		Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"node.example","port":443,"users":[{"id":"x"}]}]}}`),
	}
	raw, err := Generate([]Config{mk("a"), mk("b"), mk("c")}, GenerateOptions{
		DialerSlots: []DialerSlot{{Master: "a", Aliases: []string{"b", "c"}, Index: 0, Members: []DialerMember{member}}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Outbounds []struct {
			Tag            string         `json:"tag"`
			Settings       map[string]any `json:"settings"`
			StreamSettings struct {
				Sockopt struct {
					DialerProxy string `json:"dialerProxy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
		Routing struct {
			Balancers []json.RawMessage `json:"balancers"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	dialers, members := map[string]bool{}, 0
	wired := map[string]string{}
	for _, o := range cfg.Outbounds {
		switch {
		case strings.HasPrefix(o.Tag, "dialer-"):
			dialers[o.Tag] = true
		case strings.HasPrefix(o.Tag, SlotOutboundPrefix(0)):
			members++
		case strings.HasPrefix(o.Tag, "out-"):
			wired[o.Tag] = o.StreamSettings.Sockopt.DialerProxy
		}
	}
	for _, m := range []string{"a", "b", "c"} {
		if !dialers[DialerOutboundTag(m)] {
			t.Errorf("missing dialer outbound for %q", m)
		}
		if got := wired[OutboundTag(m)]; got != DialerOutboundTag(m) {
			t.Errorf("master %q dialerProxy = %q, want %q", m, got, DialerOutboundTag(m))
		}
	}
	if members != 1 {
		t.Errorf("slot member outbounds = %d, want 1 (shared, not copied per master)", members)
	}
	if len(cfg.Routing.Balancers) != 1 {
		t.Errorf("balancers = %d, want 1", len(cfg.Routing.Balancers))
	}
}

func TestSlotMemberKeyRoundTrip(t *testing.T) {
	for _, key := range []string{"4c439b73952a939a", "xray-nyc", "proxy-home"} {
		got, ok := SlotMemberKey(SlotMemberTag(3, key))
		if !ok || got != key {
			t.Errorf("SlotMemberKey(SlotMemberTag(3, %q)) = %q, %v", key, got, ok)
		}
	}
	for _, tag := range []string{"out-nyc", "dialer-nyc", "slot0-in", "block"} {
		if k, ok := SlotMemberKey(tag); ok {
			t.Errorf("SlotMemberKey(%q) = %q, want not a member", tag, k)
		}
	}
}

// slotBalancer returns slot 0's balancer from a generated config.
func slotBalancer(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var cfg struct {
		Routing struct {
			Balancers []map[string]any `json:"balancers"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, b := range cfg.Routing.Balancers {
		if b["tag"] == SlotBalancerTag(0) {
			return b
		}
	}
	t.Fatalf("no balancer %q", SlotBalancerTag(0))
	return nil
}

func TestGenerate_SlotPreferredBecomesCosts(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"freedom"}`)
	members := []DialerMember{{Key: "a", Outbound: ob}, {Key: "b", Outbound: ob}, {Key: "c", Outbound: ob}}
	master := Config{Name: "m", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`}

	// Without a shortlist: tolerance set, no costs, fallback = first member.
	raw, err := Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{Master: "m", Index: 0, Members: members}}})
	if err != nil {
		t.Fatal(err)
	}
	b := slotBalancer(t, raw)
	settings := b["strategy"].(map[string]any)["settings"].(map[string]any)
	if settings["tolerance"] != SlotBalancerTolerance {
		t.Errorf("tolerance = %v, want %v", settings["tolerance"], SlotBalancerTolerance)
	}
	if _, ok := settings["costs"]; ok {
		t.Errorf("costs emitted without a shortlist: %v", settings["costs"])
	}
	if b["fallbackTag"] != SlotMemberTag(0, "a") {
		t.Errorf("fallbackTag = %v, want first member", b["fallbackTag"])
	}

	// With one: each shortlisted tag costs 1 (exact regexp), then a prefix
	// catch-all demotes the rest; fallback = head of the shortlist. A
	// shortlisted key that is no longer a member is ignored.
	raw, err = Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Preferred: []string{"gone", "c", "b"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	b = slotBalancer(t, raw)
	if b["fallbackTag"] != SlotMemberTag(0, "c") {
		t.Errorf("fallbackTag = %v, want %q", b["fallbackTag"], SlotMemberTag(0, "c"))
	}
	costs := b["strategy"].(map[string]any)["settings"].(map[string]any)["costs"].([]any)
	if len(costs) != 3 {
		t.Fatalf("costs = %v, want 2 exact + 1 catch-all", costs)
	}
	for i, k := range []string{"c", "b"} {
		c := costs[i].(map[string]any)
		if c["regexp"] != true || c["match"] != "^"+SlotMemberTag(0, k)+"$" || c["value"] != float64(1) {
			t.Errorf("costs[%d] = %v, want exact match on %q with value 1", i, c, SlotMemberTag(0, k))
		}
	}
	last := costs[2].(map[string]any)
	if last["regexp"] != false || last["match"] != SlotOutboundPrefix(0) || last["value"] != slotDemotedCost {
		t.Errorf("catch-all cost = %v", last)
	}
}

// An agile slot spreads over every member it picked, and its fallback is
// the spare, not the head of the pick.
func TestGenerate_SlotExpectedAndFallback(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"freedom"}`)
	members := []DialerMember{{Key: "a", Outbound: ob}, {Key: "b", Outbound: ob}, {Key: "c", Outbound: ob}, {Key: "d", Outbound: ob}}
	master := Config{Name: "m", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`}
	raw, err := Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Strategy: StrategyAgile,
		Preferred: []string{"a", "b", "c"}, Expected: 3, Fallback: "d",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	b := slotBalancer(t, raw)
	if got := b["strategy"].(map[string]any)["settings"].(map[string]any)["expected"]; got != float64(3) {
		t.Errorf("expected = %v, want 3", got)
	}
	if b["fallbackTag"] != SlotMemberTag(0, "d") {
		t.Errorf("fallbackTag = %v, want the spare", b["fallbackTag"])
	}
	// A fallback that isn't a member is ignored.
	raw, _ = Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Preferred: []string{"b"}, Fallback: "gone",
	}}})
	if b := slotBalancer(t, raw); b["fallbackTag"] != SlotMemberTag(0, "b") {
		t.Errorf("fallbackTag = %v, want the shortlist head", b["fallbackTag"])
	}
}

// A member the path prober caught failing sorts behind every other one —
// even one the shortlist still names — and never serves as the fallback.
func TestGenerate_BrokenMembersCostLast(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"freedom"}`)
	members := []DialerMember{{Key: "a", Outbound: ob}, {Key: "b", Outbound: ob}, {Key: "c", Outbound: ob}}
	master := Config{Name: "m", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`}

	raw, err := Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Preferred: []string{"a", "b"}, Broken: []string{"a"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	b := slotBalancer(t, raw)
	if b["fallbackTag"] != SlotMemberTag(0, "b") {
		t.Errorf("fallbackTag = %v, want the first unbroken shortlisted member", b["fallbackTag"])
	}
	costs := b["strategy"].(map[string]any)["settings"].(map[string]any)["costs"].([]any)
	first := costs[0].(map[string]any)
	if first["match"] != "^"+SlotMemberTag(0, "a")+"$" || first["value"] != slotBrokenCost {
		t.Errorf("costs[0] = %v, want broken a at %v ahead of its shortlist cost", first, slotBrokenCost)
	}

	// No shortlist: the broken cost alone, and the fallback skips it.
	raw, _ = Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Broken: []string{"a"},
	}}})
	b = slotBalancer(t, raw)
	if b["fallbackTag"] != SlotMemberTag(0, "b") {
		t.Errorf("fallbackTag = %v, want first unbroken member", b["fallbackTag"])
	}
	if costs := b["strategy"].(map[string]any)["settings"].(map[string]any)["costs"].([]any); len(costs) != 1 {
		t.Errorf("costs = %v, want only the broken member's", costs)
	}

	// Every member broken: plain ranking, nothing singled out.
	raw, _ = Generate([]Config{master}, GenerateOptions{DialerSlots: []DialerSlot{{
		Master: "m", Index: 0, Members: members, Broken: []string{"a", "b", "c"},
	}}})
	b = slotBalancer(t, raw)
	if _, ok := b["strategy"].(map[string]any)["settings"].(map[string]any)["costs"]; ok {
		t.Errorf("costs emitted with every member broken")
	}
	if b["fallbackTag"] != SlotMemberTag(0, "a") {
		t.Errorf("fallbackTag = %v, want first member", b["fallbackTag"])
	}
}

// The probe inbound routes each username to its outbound — every slot
// member and every master chained over each member — ahead of user rules,
// and fails closed for anything else.
func TestGenerate_ProbeInboundRoutes(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"freedom"}`)
	members := []DialerMember{{Key: "a", Outbound: ob}, {Key: "b", Outbound: ob}}
	entries := []Config{
		{Name: "m1", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vmess","settings":{"vnext":[{"address":"1.2.3.4","port":1,"users":[{"id":"x"}]}]}}`},
		{Name: "m2", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`},
	}
	raw, err := Generate(entries, GenerateOptions{
		RoutingRules: `[{"type":"field","network":"tcp,udp","outboundTag":"direct"}]`,
		DialerSlots:  []DialerSlot{{Master: "m1", Aliases: []string{"m2"}, Index: 0, Members: members}},
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := outboundsByTag(t, raw)
	for _, m := range []string{"m1", "m2"} {
		for _, k := range []string{"a", "b"} {
			tag := ChainProbeTag(0, m, k)
			if got := dialerProxyOf(obs[tag]); got != SlotMemberTag(0, k) {
				t.Errorf("%s dialerProxy = %q, want the one member %q", tag, got, SlotMemberTag(0, k))
			}
		}
	}
	if got := dialerProxyOf(obs["out-m1"]); got != DialerOutboundTag("m1") {
		t.Errorf("the master itself still dials through its slot: %q", got)
	}

	var cfg struct {
		Inbounds []struct {
			Tag      string         `json:"tag"`
			Port     int            `json:"port"`
			Settings map[string]any `json:"settings"`
		} `json:"inbounds"`
		Routing struct {
			Rules []map[string]any `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	var accounts []any
	for _, in := range cfg.Inbounds {
		if in.Tag == ProbeTag {
			if in.Port != ProbePort || in.Settings["auth"] != "password" {
				t.Errorf("probe inbound = %+v", in)
			}
			accounts = in.Settings["accounts"].([]any)
		}
	}
	if len(accounts) != 2+2*2 {
		t.Fatalf("accounts = %d, want 2 members + 2 masters × 2 members", len(accounts))
	}
	// api rule, then 6 probe routes + the fail-closed catch-all, then the
	// user's catch-all.
	rules := cfg.Routing.Rules
	if len(rules) < 9 {
		t.Fatalf("rules = %v", rules)
	}
	for i := 1; i <= 7; i++ {
		if in := rules[i]["inboundTag"].([]any); in[0] != ProbeTag {
			t.Errorf("rules[%d] = %v, want a probe rule", i, rules[i])
		}
	}
	if rules[1]["user"].([]any)[0] != rules[1]["outboundTag"] {
		t.Errorf("probe rule routes user %v to %v", rules[1]["user"], rules[1]["outboundTag"])
	}
	if rules[7]["outboundTag"] != TagBlock || rules[7]["user"] != nil {
		t.Errorf("probe catch-all = %v, want block", rules[7])
	}
	if rules[8]["outboundTag"] != "direct" {
		t.Errorf("user rule moved: rules[8] = %v", rules[8])
	}
	// Chain tags must stay out of the observatory's prefix and the slot
	// member pattern.
	for tag := range obs {
		if strings.HasPrefix(tag, "chain") {
			if strings.HasPrefix(tag, ObservatorySelectorPrefix) {
				t.Errorf("%s matches the observatory selector", tag)
			}
			if _, ok := SlotMemberKey(tag); ok {
				t.Errorf("%s parses as a slot member", tag)
			}
		}
	}
}
