package xray

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const tlsVless = `{"protocol":"vless","settings":{"vnext":[{"address":"example.com","port":443,"users":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`

func TestFragmentNormalize(t *testing.T) {
	f, err := Fragment{}.Normalize()
	if err != nil || f != DefaultFragment {
		t.Fatalf("empty = %+v, %v; want defaults", f, err)
	}
	f, err = Fragment{Packets: " 1-3", Length: "10 - 20", Interval: "0"}.Normalize()
	if err != nil || f != (Fragment{Packets: "1-3", Length: "10-20", Interval: "0"}) {
		t.Fatalf("custom = %+v, %v", f, err)
	}
	for _, bad := range []Fragment{
		{Packets: "hello"},
		{Packets: "0-2"},
		{Length: "0-10"},
		{Length: "20-10"},
		{Interval: "x"},
	} {
		if _, err := bad.Normalize(); !errors.Is(err, ErrInvalidFragment) {
			t.Errorf("%+v: err = %v, want ErrInvalidFragment", bad, err)
		}
	}
}

func TestFragmentSupport(t *testing.T) {
	cases := []struct {
		name, ob string
		f        Fragment
		ok       bool
	}{
		{"vless tls", tlsVless, DefaultFragment, true},
		{"reality", `{"protocol":"vless","streamSettings":{"security":"reality"}}`, DefaultFragment, true},
		{"no tls hello", `{"protocol":"vmess","streamSettings":{"network":"ws"}}`, DefaultFragment, false},
		{"no tls range", `{"protocol":"vmess","streamSettings":{"network":"ws"}}`, Fragment{Packets: "1-3", Length: "1-5", Interval: "1"}, true},
		{"kcp", `{"protocol":"vmess","streamSettings":{"network":"kcp","security":"tls"}}`, DefaultFragment, false},
		{"wireguard", `{"protocol":"wireguard"}`, DefaultFragment, false},
		{"own dialer", `{"protocol":"trojan","streamSettings":{"security":"tls","sockopt":{"dialerProxy":"x"}}}`, DefaultFragment, false},
		{"garbage", `{`, DefaultFragment, false},
	}
	for _, tc := range cases {
		ok, why := FragmentSupport(tc.ob, false, tc.f)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v (%q), want %v", tc.name, ok, why, tc.ok)
		}
		if !ok && why == "" {
			t.Errorf("%s: unsupported without a reason", tc.name)
		}
	}
	if ok, _ := FragmentSupport(`{"protocol":"freedom"}`, true, DefaultFragment); !ok {
		t.Error("a master is judged by its pool, not its own outbound")
	}
}

func outboundsByTag(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	var c struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, ob := range c.Outbounds {
		out[ob["tag"].(string)] = ob
	}
	return out
}

func dialerProxyOf(ob map[string]any) string {
	ss, _ := ob["streamSettings"].(map[string]any)
	sock, _ := ss["sockopt"].(map[string]any)
	s, _ := sock["dialerProxy"].(string)
	return s
}

func TestGenerateFragment(t *testing.T) {
	def := DefaultFragment.Encode()
	custom := Fragment{Packets: "1-2", Length: "5-10", Interval: "1-2"}
	entries := []Config{
		{Name: "a", SocksPort: 11900, Enabled: true, Fragment: def, Outbound: tlsVless},
		{Name: "b", SocksPort: 11901, Enabled: true, Fragment: def, Outbound: tlsVless},
		{Name: "off", SocksPort: 11902, Enabled: true, Outbound: tlsVless},
		{Name: "plain", SocksPort: 11903, Enabled: true, Fragment: def, Outbound: `{"protocol":"vless","settings":{"vnext":[{"address":"example.com","port":80,"users":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"}]}]}}`},
		{Name: "m", SocksPort: 11904, Enabled: true, Dialer: "xray:off", Fragment: custom.Encode(), Outbound: tlsVless},
	}
	slots := []DialerSlot{{Master: "m", Index: 0, Members: []DialerMember{
		{Key: "n1", Outbound: json.RawMessage(tlsVless)},
		{Key: "n2", Outbound: json.RawMessage(`{"protocol":"shadowsocks","settings":{"servers":[{"address":"1.2.3.4","port":8388,"method":"aes-128-gcm","password":"x"}]}}`)},
	}}}
	raw, err := Generate(entries, GenerateOptions{DialerSlots: slots})
	if err != nil {
		t.Fatal(err)
	}
	obs := outboundsByTag(t, raw)
	defTag, customTag := FragmentOutboundTag(DefaultFragment), FragmentOutboundTag(custom)

	if got := dialerProxyOf(obs["out-a"]); got != defTag {
		t.Errorf("a dialerProxy = %q, want %q", got, defTag)
	}
	if got := dialerProxyOf(obs["out-b"]); got != defTag {
		t.Errorf("b shares the outbound: dialerProxy = %q, want %q", got, defTag)
	}
	if got := dialerProxyOf(obs["out-off"]); got != "" {
		t.Errorf("off: dialerProxy = %q, want none", got)
	}
	if got := dialerProxyOf(obs["out-plain"]); got != "" {
		t.Errorf("plain (no TLS, tlshello): dialerProxy = %q, want none", got)
	}
	if got := dialerProxyOf(obs["out-m"]); got != DialerOutboundTag("m") {
		t.Errorf("master keeps its dialer chain: dialerProxy = %q", got)
	}
	if got := dialerProxyOf(obs[SlotMemberTag(0, "n1")]); got != customTag {
		t.Errorf("TLS pool node: dialerProxy = %q, want master's %q", got, customTag)
	}
	if got := dialerProxyOf(obs[SlotMemberTag(0, "n2")]); got != customTag {
		t.Errorf("packet-range fragment applies without TLS too: dialerProxy = %q", got)
	}
	for _, tag := range []string{defTag, customTag} {
		ob, ok := obs[tag]
		if !ok || ob["protocol"] != "freedom" {
			t.Fatalf("missing freedom outbound %s: %v", tag, ob)
		}
	}
	if len(obs) != 2+5+2+1+2+2 { // block, direct, entries, slot members, dialer, frags, chain probes
		t.Errorf("outbounds = %d, want one freedom per distinct setting", len(obs))
	}

	// The real binary must accept it. Needs EMWALL_XRAY_BIN or the
	// installed one; skipped otherwise.
	if testing.Short() {
		return
	}
	bin := os.Getenv("EMWALL_XRAY_BIN")
	if bin == "" {
		bin = "/usr/local/bin/em-wall-xray"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Logf("no xray binary at %s; skipping config check", bin)
		return
	}
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("xray rejects config: %v\n%s", err, out)
	}
}
