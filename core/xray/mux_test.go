package xray

import (
	"encoding/json"
	"testing"
)

func TestMuxSupport(t *testing.T) {
	cases := []struct {
		name, ob string
		ok       bool
	}{
		{"vmess ws", `{"protocol":"vmess","streamSettings":{"network":"ws"}}`, true},
		{"vmess default tcp", `{"protocol":"vmess"}`, true},
		{"trojan tcp", `{"protocol":"trojan","streamSettings":{"network":"tcp"}}`, true},
		{"vless no flow", `{"protocol":"vless","settings":{"vnext":[{"users":[{"id":"x"}]}]}}`, true},
		{"vless vision", `{"protocol":"vless","settings":{"vnext":[{"users":[{"id":"x","flow":"xtls-rprx-vision"}]}]}}`, false},
		{"vmess grpc", `{"protocol":"vmess","streamSettings":{"network":"grpc"}}`, false},
		{"vless xhttp", `{"protocol":"vless","streamSettings":{"network":"xhttp"}}`, false},
		{"shadowsocks", `{"protocol":"shadowsocks"}`, false},
		{"user mux", `{"protocol":"vmess","mux":{"enabled":false}}`, false},
		{"garbage", `{`, false},
	}
	for _, tc := range cases {
		ok, why := MuxSupport(tc.ob)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v (%q), want %v", tc.name, ok, why, tc.ok)
		}
		if !ok && why == "" {
			t.Errorf("%s: unsupported without a reason", tc.name)
		}
	}
}

func TestGenerateAppliesMuxOnlyWhenOptedInAndSupported(t *testing.T) {
	entries := []Config{
		{Name: "on", SocksPort: 11800, Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"ws"}}`},
		{Name: "off", SocksPort: 11801, Enabled: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"ws"}}`},
		{Name: "grpc", SocksPort: 11802, Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"grpc"}}`},
		{Name: "own", SocksPort: 11803, Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","mux":{"enabled":true,"concurrency":2}}`},
	}
	raw, err := Generate(entries, GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	mux := func(name string) map[string]any {
		m, _ := outboundByTag(cfg.Outbounds, OutboundTag(name))["mux"].(map[string]any)
		return m
	}
	if m := mux("on"); m == nil || m["enabled"] != true || m["xudpProxyUDP443"] != "skip" {
		t.Errorf("on: mux = %v", m)
	}
	if mux("off") != nil || mux("grpc") != nil {
		t.Errorf("mux applied where it must not be")
	}
	if m := mux("own"); m == nil || m["concurrency"] != float64(2) {
		t.Errorf("user's own mux block was overwritten: %v", m)
	}
}
