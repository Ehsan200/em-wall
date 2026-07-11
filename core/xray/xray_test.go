package xray

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseInterface(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"xray:work", []string{"work"}},
		{"xray:work,home", []string{"work", "home"}},
		{"xray: work , home ", []string{"work", "home"}},
		{"xray:,,", nil}, // empty entries dropped
		{"xray:", nil},
		{"proxy:work", nil},
		{"utun3", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseInterface(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseInterface(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseInterface(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestInternalProxyName(t *testing.T) {
	cases := map[string]string{
		"foo":     "_xray_foo",
		"  FOO  ": "_xray_foo",
		"my-bar":  "_xray_my-bar",
	}
	for in, want := range cases {
		if got := InternalProxyName(in); got != want {
			t.Errorf("InternalProxyName(%q) = %q, want %q", in, got, want)
		}
		if !IsInternalProxyName(want) {
			t.Errorf("IsInternalProxyName(%q) = false, want true", want)
		}
	}
	if IsInternalProxyName("regular") {
		t.Errorf("IsInternalProxyName(%q) = true, want false", "regular")
	}
}

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"work":     true,
		"my-pipe":  true,
		"home_2":   true,
		"UPPER":    true, // normalized to lower in ValidName
		"":         false,
		"  ":       false,
		"has dot.": false,
		"with sp":  false,
	}
	for in, want := range cases {
		if got := ValidName(in); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGenerateRoundTrip(t *testing.T) {
	entries := []Config{
		{Name: "alpha", SocksPort: 11800, Enabled: true,
			Outbound: `{"protocol":"freedom","settings":{}}`},
		{Name: "beta", SocksPort: 11801, Enabled: true,
			Outbound: `{"protocol":"vless","tag":"user-set","settings":{"vnext":[]}}`},
		{Name: "disabled-zulu", SocksPort: 11802, Enabled: false,
			Outbound: `{"protocol":"freedom"}`},
	}

	raw, err := Generate(entries, GenerateOptions{LogDir: "/tmp/logs"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Log      map[string]any `json:"log"`
		Inbounds []struct {
			Tag      string `json:"tag"`
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []map[string]any `json:"outbounds"`
		Routing   struct {
			DomainStrategy string `json:"domainStrategy"`
			Rules          []struct {
				Type        string   `json:"type"`
				InboundTag  []string `json:"inboundTag"`
				OutboundTag string   `json:"outboundTag"`
			} `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal generated config: %v\n%s", err, raw)
	}

	// Disabled entries should not appear.
	if len(cfg.Inbounds) != 2 {
		t.Fatalf("inbounds: got %d, want 2 (disabled-zulu must be skipped)", len(cfg.Inbounds))
	}

	// Inbounds sorted by entry name (alpha < beta).
	if cfg.Inbounds[0].Tag != InboundTag("alpha") || cfg.Inbounds[1].Tag != InboundTag("beta") {
		t.Errorf("inbound tag order = %v %v, want sorted by name",
			cfg.Inbounds[0].Tag, cfg.Inbounds[1].Tag)
	}
	if cfg.Inbounds[0].Listen != "127.0.0.1" || cfg.Inbounds[0].Port != 11800 {
		t.Errorf("inbound[0] = %s:%d, want 127.0.0.1:11800",
			cfg.Inbounds[0].Listen, cfg.Inbounds[0].Port)
	}
	if cfg.Inbounds[0].Protocol != "socks" {
		t.Errorf("inbound[0].protocol = %q, want socks", cfg.Inbounds[0].Protocol)
	}

	// Each enabled entry produces one outbound, with the tag
	// overridden to match the routing rule. Plus direct + block are
	// always appended so user rules can reference them.
	if len(cfg.Outbounds) != 4 {
		t.Fatalf("outbounds: got %d, want 4 (2 entries + direct + block)", len(cfg.Outbounds))
	}
	if got := cfg.Outbounds[0]["tag"]; got != OutboundTag("alpha") {
		t.Errorf("outbound[0].tag = %v, want %q", got, OutboundTag("alpha"))
	}
	// User-set tag on beta should be overridden.
	if got := cfg.Outbounds[1]["tag"]; got != OutboundTag("beta") {
		t.Errorf("outbound[1].tag = %v (user-set tag must be overridden), want %q",
			got, OutboundTag("beta"))
	}

	// Routing rules pair inbound→outbound 1:1.
	if len(cfg.Routing.Rules) != 2 {
		t.Fatalf("routing rules: got %d, want 2", len(cfg.Routing.Rules))
	}
	for i, r := range cfg.Routing.Rules {
		if len(r.InboundTag) != 1 || !strings.HasPrefix(r.InboundTag[0], "in-") {
			t.Errorf("rule[%d].inboundTag = %v, want one 'in-NAME'", i, r.InboundTag)
		}
		expectOut := "out-" + strings.TrimPrefix(r.InboundTag[0], "in-")
		if r.OutboundTag != expectOut {
			t.Errorf("rule[%d] tag pair mismatch: inbound=%s outbound=%s",
				i, r.InboundTag[0], r.OutboundTag)
		}
	}

	if cfg.Log == nil {
		t.Errorf("log block missing despite non-empty logDir")
	}
}

func TestGenerateNoEnabledFallback(t *testing.T) {
	raw, err := Generate(nil, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate(nil): %v", err)
	}
	var cfg struct {
		Inbounds  []any            `json:"inbounds"`
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(cfg.Inbounds) != 0 {
		t.Errorf("inbounds with no entries: got %d, want 0", len(cfg.Inbounds))
	}
	// With no entries the config still ships direct + block so user
	// rules can reference them and xray has at least one outbound to
	// satisfy its startup check.
	if len(cfg.Outbounds) != 2 {
		t.Fatalf("fallback outbounds: got %d, want 2 (direct + block)", len(cfg.Outbounds))
	}
}

func TestGenerateRejectsInvalidOutbound(t *testing.T) {
	_, err := Generate([]Config{{
		Name: "bad", SocksPort: 11800, Enabled: true,
		Outbound: `not-json`,
	}}, GenerateOptions{})
	if err == nil {
		t.Fatalf("Generate accepted invalid outbound JSON, want error")
	}
}

func TestGenerateAlwaysIncludesDirectAndBlock(t *testing.T) {
	raw, err := Generate(nil, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	have := map[string]string{}
	for _, ob := range cfg.Outbounds {
		tag, _ := ob["tag"].(string)
		proto, _ := ob["protocol"].(string)
		have[tag] = proto
	}
	if have[TagDirect] != "freedom" {
		t.Errorf("missing direct outbound: got %v", have)
	}
	if have[TagBlock] != "blackhole" {
		t.Errorf("missing block outbound: got %v", have)
	}
}

func TestGenerateUserRulesPrecedeAutoPairs(t *testing.T) {
	entries := []Config{
		{Name: "alpha", SocksPort: 11800, Enabled: true,
			Outbound: `{"protocol":"freedom"}`},
	}
	user := `[{"type":"field","domain":["geosite:ads-all"],"outboundTag":"block"}]`
	raw, err := Generate(entries, GenerateOptions{RoutingRules: user})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Routing struct {
			Rules []map[string]any `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(cfg.Routing.Rules) < 2 {
		t.Fatalf("expected user rule + per-entry pair, got %d", len(cfg.Routing.Rules))
	}
	if cfg.Routing.Rules[0]["outboundTag"] != "block" {
		t.Errorf("first rule is not the user rule: %v", cfg.Routing.Rules[0])
	}
	if cfg.Routing.Rules[1]["outboundTag"] != OutboundTag("alpha") {
		t.Errorf("second rule is not the auto pair: %v", cfg.Routing.Rules[1])
	}
}

func TestGenerateHealsXHTTPExtraString(t *testing.T) {
	// Legacy stored shape: xhttpSettings.extra is a JSON string
	// (instead of the object xray actually expects). The supervisor
	// regenerates the config on every restart, so Generate must
	// auto-heal this or xray would refuse to start with:
	//   "Failed to unmarshal "extra": cannot unmarshal string into
	//   Go value of type conf.SplitHTTPConfig"
	bad := `{
        "protocol":"vless",
        "settings":{"vnext":[]},
        "streamSettings":{
            "network":"xhttp",
            "xhttpSettings":{"path":"/x","extra":"{\"maxConcurrentUploads\":\"100-200\"}"}
        }
    }`
	raw, err := Generate([]Config{{
		Name: "alpha", SocksPort: 11800, Enabled: true, Outbound: bad,
	}}, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	first := cfg.Outbounds[0]
	ss, _ := first["streamSettings"].(map[string]any)
	xh, _ := ss["xhttpSettings"].(map[string]any)
	if _, ok := xh["extra"].(map[string]any); !ok {
		t.Fatalf("xhttpSettings.extra was not healed to an object: %T = %v",
			xh["extra"], xh["extra"])
	}

	// And: an invalid JSON string in extra should be dropped so xray
	// at least starts.
	bad2 := `{
        "protocol":"vless",
        "settings":{},
        "streamSettings":{"network":"xhttp","xhttpSettings":{"path":"/x","extra":"not json"}}
    }`
	raw, err = Generate([]Config{{
		Name: "beta", SocksPort: 11801, Enabled: true, Outbound: bad2,
	}}, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate beta: %v", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal beta: %v", err)
	}
	xh, _ = cfg.Outbounds[0]["streamSettings"].(map[string]any)["xhttpSettings"].(map[string]any)
	if _, present := xh["extra"]; present {
		t.Errorf("invalid extra string should have been dropped, got %v", xh["extra"])
	}
}

func TestValidateRoutingRules(t *testing.T) {
	cases := map[string]bool{
		"":                     true,
		"[]":                   true,
		`[{"type":"field"}]`:   true,
		`[{"a":1},{"b":2}]`:    true,
		`not-json`:             false,
		`{}`:                   false, // object, not array
		`[1, 2, 3]`:            false,
		`["string"]`:           false,
		`[{"ok":true}, "bad"]`: false,
	}
	for in, ok := range cases {
		err := ValidateRoutingRules(in)
		if ok && err != nil {
			t.Errorf("ValidateRoutingRules(%q) = %v, want nil", in, err)
		}
		if !ok && err == nil {
			t.Errorf("ValidateRoutingRules(%q) = nil, want error", in)
		}
	}
}
