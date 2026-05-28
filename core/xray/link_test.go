package xray

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func mustParseOutbound(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("outbound JSON: %v\n%s", err, raw)
	}
	return m
}

func TestSuggestName(t *testing.T) {
	cases := map[string]string{
		"":                 "imported",
		"My Server #1":     "my-server-1",
		"   spaces  here ": "spaces--here",
		"INVALID!!":        "invalid",
		"---":              "imported",
	}
	for in, want := range cases {
		if got := suggestName(in); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLinkRouting(t *testing.T) {
	cases := []string{
		"vless://uuid@example.com:443?security=tls&type=ws&path=/x#vless-test",
		"trojan://pw@example.com:443?security=tls&sni=foo.com#trojan-test",
	}
	for _, link := range cases {
		_, _, err := ParseLink(link)
		if err != nil {
			t.Errorf("ParseLink(%q) failed: %v", link, err)
		}
	}

	if _, _, err := ParseLink(""); err == nil {
		t.Errorf("ParseLink(\"\") = nil err, want error")
	}
	if _, _, err := ParseLink("unknown://x"); err == nil {
		t.Errorf("ParseLink(unknown scheme) = nil err, want error")
	}
}

func TestParseVLESS(t *testing.T) {
	name, raw, err := ParseLink("vless://abc-123@host.example:443?security=tls&type=ws&path=/p&host=sni.example&fp=chrome#My VLESS")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	if name != "my-vless" {
		t.Errorf("name = %q, want my-vless", name)
	}
	ob := mustParseOutbound(t, raw)
	if ob["protocol"] != "vless" {
		t.Errorf("protocol = %v, want vless", ob["protocol"])
	}
	vnext := ob["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)
	if vnext["address"] != "host.example" {
		t.Errorf("address = %v", vnext["address"])
	}
	if vnext["port"].(float64) != 443 {
		t.Errorf("port = %v", vnext["port"])
	}
	user := vnext["users"].([]any)[0].(map[string]any)
	if user["id"] != "abc-123" {
		t.Errorf("uuid = %v", user["id"])
	}
	if user["encryption"] != "none" {
		t.Errorf("default encryption = %v, want none", user["encryption"])
	}
	ss := ob["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Errorf("streamSettings basics: %v", ss)
	}
	if ss["wsSettings"].(map[string]any)["path"] != "/p" {
		t.Errorf("ws path = %v", ss["wsSettings"])
	}
	tls := ss["tlsSettings"].(map[string]any)
	if tls["serverName"] != "sni.example" {
		t.Errorf("sni preferred over host: %v", tls)
	}
	if tls["fingerprint"] != "chrome" {
		t.Errorf("fingerprint = %v", tls)
	}
}

func TestParseVMESS(t *testing.T) {
	payload := `{
        "v":"2","ps":"vmess host",
        "add":"host.example","port":"8443",
        "id":"uuid-here","aid":0,"scy":"auto",
        "net":"ws","host":"vmess-host","path":"/v","tls":"tls","sni":"sni.example"
    }`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	link := "vmess://" + encoded

	name, raw, err := ParseLink(link)
	if err != nil {
		t.Fatalf("ParseLink vmess: %v", err)
	}
	if name != "vmess-host" {
		t.Errorf("name = %q, want vmess-host", name)
	}
	ob := mustParseOutbound(t, raw)
	if ob["protocol"] != "vmess" {
		t.Errorf("protocol = %v", ob["protocol"])
	}
	vnext := ob["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)
	if vnext["address"] != "host.example" || vnext["port"].(float64) != 8443 {
		t.Errorf("vnext host:port = %v:%v", vnext["address"], vnext["port"])
	}
	user := vnext["users"].([]any)[0].(map[string]any)
	if user["id"] != "uuid-here" || user["security"] != "auto" {
		t.Errorf("user = %v", user)
	}
	ss := ob["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Errorf("stream basics = %v", ss)
	}
	if ss["tlsSettings"].(map[string]any)["serverName"] != "sni.example" {
		t.Errorf("sni = %v", ss["tlsSettings"])
	}
	if !strings.Contains(raw, "\"Host\"") {
		t.Errorf("ws Host header missing in output:\n%s", raw)
	}
}

func TestParseTrojanDefaultsTLS(t *testing.T) {
	_, raw, err := ParseLink("trojan://my-pass@trojan.example:443?sni=t.example#trojan-default-tls")
	if err != nil {
		t.Fatalf("ParseLink trojan: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	if ob["protocol"] != "trojan" {
		t.Errorf("protocol = %v", ob["protocol"])
	}
	servers := ob["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if servers["password"] != "my-pass" {
		t.Errorf("password lost: %v", servers)
	}
	ss := ob["streamSettings"].(map[string]any)
	if ss["security"] != "tls" {
		t.Errorf("trojan should default to security=tls, got %v", ss["security"])
	}
}

func TestParseLinkAcceptsFlatVLESSJSON(t *testing.T) {
	// The shape some panels (3X-UI-style) export: address/port/id on
	// a flat settings object, no vnext wrapper, no top-level
	// decryption. Must be reshaped into canonical xray before xray
	// will start.
	flat := `{
        "protocol": "vless",
        "settings": {
            "address": "host.example",
            "port": 443,
            "id": "00000000-0000-0000-0000-000000000000",
            "flow": "",
            "encryption": "none"
        },
        "tag": "Some Imported Server",
        "streamSettings": {
            "network": "xhttp",
            "security": "tls",
            "tlsSettings": {"serverName": "host.example"},
            "xhttpSettings": {"path": "/path","mode": "packet-up"}
        }
    }`
	name, raw, err := ParseLink(flat)
	if err != nil {
		t.Fatalf("ParseLink(flat-json): %v", err)
	}
	if name != "some-imported-server" {
		t.Errorf("name from tag = %q, want some-imported-server", name)
	}
	ob := mustParseOutbound(t, raw)
	settings, ok := ob["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing")
	}
	if settings["decryption"] != "none" {
		t.Errorf("decryption not defaulted to \"none\": %v", settings["decryption"])
	}
	vnext, ok := settings["vnext"].([]any)
	if !ok || len(vnext) != 1 {
		t.Fatalf("vnext not synthesized: %v", settings["vnext"])
	}
	v0 := vnext[0].(map[string]any)
	if v0["address"] != "host.example" {
		t.Errorf("address lost in reshape: %v", v0)
	}
	if v0["port"].(float64) != 443 {
		t.Errorf("port lost in reshape: %v", v0["port"])
	}
	user := v0["users"].([]any)[0].(map[string]any)
	if user["id"] != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("uuid lost in reshape: %v", user)
	}
	if user["encryption"] != "none" {
		t.Errorf("encryption lost in reshape: %v", user["encryption"])
	}
	if user["flow"] != "" {
		t.Errorf("flow not preserved (even empty): %v", user["flow"])
	}
	// streamSettings should round-trip untouched.
	if ob["streamSettings"].(map[string]any)["network"] != "xhttp" {
		t.Errorf("streamSettings munged: %v", ob["streamSettings"])
	}
}

func TestParseLinkCanonicalJSONPassesThrough(t *testing.T) {
	// Already-canonical input keeps its vnext, but vless gets the
	// required settings.decryption added if missing.
	canon := `{
        "protocol":"vless",
        "settings":{"vnext":[{"address":"h.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},
        "tag":"keep"
    }`
	_, raw, err := ParseLink(canon)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	s := ob["settings"].(map[string]any)
	if s["decryption"] != "none" {
		t.Errorf("canonical vless without decryption should be filled in, got %v", s["decryption"])
	}
	if len(s["vnext"].([]any)) != 1 {
		t.Errorf("vnext mutated: %v", s["vnext"])
	}
}

func TestParseLinkFlatTrojanJSON(t *testing.T) {
	flat := `{
        "protocol":"trojan",
        "settings":{"address":"t.example","port":443,"password":"hunter2"},
        "tag":"t-flat"
    }`
	_, raw, err := ParseLink(flat)
	if err != nil {
		t.Fatalf("ParseLink trojan flat: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	servers := ob["settings"].(map[string]any)["servers"].([]any)
	s0 := servers[0].(map[string]any)
	if s0["address"] != "t.example" || s0["password"] != "hunter2" || s0["port"].(float64) != 443 {
		t.Errorf("flat trojan reshape lost fields: %v", s0)
	}
}

func TestParseLinkRejectsBadJSON(t *testing.T) {
	if _, _, err := ParseLink("{not json"); err == nil {
		t.Errorf("ParseLink({not json) = nil err, want error")
	}
	if _, _, err := ParseLink(`{"foo":"bar"}`); err == nil {
		t.Errorf("ParseLink(no protocol) = nil err, want error")
	}
}

func TestParseSSSIP002(t *testing.T) {
	// SIP002: ss://BASE64(method:pass)@host:port#name
	userinfo := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	link := "ss://" + userinfo + "@ss.example:8388#shadow"
	name, raw, err := ParseLink(link)
	if err != nil {
		t.Fatalf("ParseLink ss: %v", err)
	}
	if name != "shadow" {
		t.Errorf("name = %q", name)
	}
	ob := mustParseOutbound(t, raw)
	servers := ob["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if servers["method"] != "aes-256-gcm" || servers["password"] != "secret" {
		t.Errorf("ss method/password lost: %v", servers)
	}
	if servers["address"] != "ss.example" || servers["port"].(float64) != 8388 {
		t.Errorf("ss host:port lost: %v", servers)
	}
}

func TestParseVLESSXHTTP(t *testing.T) {
	// xhttp share link with mode + host + path.
	_, raw, err := ParseLink("vless://uuid@host.example:443?security=tls&type=xhttp&path=/up&host=front.example&mode=auto#xh")
	if err != nil {
		t.Fatalf("ParseLink xhttp: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	ss := ob["streamSettings"].(map[string]any)
	if ss["network"] != "xhttp" {
		t.Errorf("network = %v, want xhttp", ss["network"])
	}
	x := ss["xhttpSettings"].(map[string]any)
	if x["host"] != "front.example" || x["path"] != "/up" || x["mode"] != "auto" {
		t.Errorf("xhttpSettings = %v", x)
	}
}

func TestParseVLESSXHTTPExtraDecodesAsObject(t *testing.T) {
	// The "extra" query param is a URL-encoded JSON object — xray's
	// conf.SplitHTTPConfig refuses a raw string. Round-trip the JSON
	// here to make sure the parser unwraps it.
	extra := `{"maxConcurrentUploads":"100-200","scMaxEachPostBytes":"500000"}`
	link := "vless://uuid@host.example:443?security=tls&type=xhttp&path=/x&extra=" + url.QueryEscape(extra) + "#xh-extra"
	_, raw, err := ParseLink(link)
	if err != nil {
		t.Fatalf("ParseLink xhttp+extra: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	ss := ob["streamSettings"].(map[string]any)
	x := ss["xhttpSettings"].(map[string]any)
	switch e := x["extra"].(type) {
	case map[string]any:
		if e["maxConcurrentUploads"] != "100-200" {
			t.Errorf("xhttp.extra missing fields: %v", e)
		}
	default:
		t.Fatalf("xhttp.extra type = %T, want map[string]any (xray expects an object)", e)
	}
}

func TestParseVLESSSplitHTTPAlias(t *testing.T) {
	// Older share links use type=splithttp — must normalize to xhttp.
	_, raw, err := ParseLink("vless://uuid@host.example:443?security=tls&type=splithttp&path=/sp#sp")
	if err != nil {
		t.Fatalf("ParseLink splithttp: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	ss := ob["streamSettings"].(map[string]any)
	if ss["network"] != "xhttp" {
		t.Errorf("splithttp not normalized to xhttp: network=%v", ss["network"])
	}
	if _, ok := ss["xhttpSettings"]; !ok {
		t.Errorf("xhttpSettings missing under splithttp alias")
	}
}

func TestParseSSLegacy(t *testing.T) {
	// Legacy: ss://BASE64(method:pass@host:port)#name
	full := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:hunter2@ss.example:443"))
	link := "ss://" + full + "#legacy"
	_, raw, err := ParseLink(link)
	if err != nil {
		t.Fatalf("ParseLink ss legacy: %v", err)
	}
	ob := mustParseOutbound(t, raw)
	servers := ob["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if servers["method"] != "chacha20-ietf-poly1305" || servers["password"] != "hunter2" {
		t.Errorf("ss legacy creds lost: %v", servers)
	}
	if servers["address"] != "ss.example" || servers["port"].(float64) != 443 {
		t.Errorf("ss legacy host:port lost: %v", servers)
	}
}
