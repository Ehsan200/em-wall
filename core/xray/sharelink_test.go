package xray

import (
	"reflect"
	"strings"
	"testing"
)

// A link that ParseLink reads, rebuilt by BuildLink, must parse back to
// the same outbound.
func TestBuildLinkRoundTrip(t *testing.T) {
	links := []string{
		"vless://abc-123@host.example:443?security=tls&type=ws&path=/p&host=sni.example&fp=chrome#a",
		"vless://abc-123@1.2.3.4:443?security=reality&type=tcp&sni=www.x.com&pbk=PUB&sid=ab12&fp=chrome&flow=xtls-rprx-vision#b",
		"vless://abc-123@h.example:443?security=tls&type=grpc&serviceName=svc&alpn=h2,http/1.1#c",
		`vless://abc-123@h.example:443?security=tls&type=xhttp&path=/x&mode=auto&extra={"xPaddingBytes":"100-1000"}#d`,
		"vless://abc-123@[2001:db8::1]:8443?type=tcp#e",
		"trojan://pw@example.com:443?security=tls&sni=foo.com#f",
		"ss://YWVzLTI1Ni1nY206c2VjcmV0@ss.example:8388#g",
	}
	for _, l := range links {
		_, ob, err := ParseLink(l)
		if err != nil {
			t.Fatalf("ParseLink(%q): %v", l, err)
		}
		built, notes, err := BuildLink("my-node", ob)
		if err != nil {
			t.Fatalf("BuildLink(%q): %v", l, err)
		}
		if len(notes) != 0 {
			t.Errorf("BuildLink(%q) notes = %v, want none", l, notes)
		}
		name, ob2, err := ParseLink(built)
		if err != nil {
			t.Fatalf("ParseLink(rebuilt %q): %v", built, err)
		}
		if name != "my-node" {
			t.Errorf("rebuilt %q name = %q", built, name)
		}
		if a, b := mustParseOutbound(t, ob), mustParseOutbound(t, ob2); !reflect.DeepEqual(a, b) {
			t.Errorf("round trip of %q via %q:\n got %v\nwant %v", l, built, b, a)
		}
	}
}

func TestBuildLinkVMESSRoundTrip(t *testing.T) {
	ob := `{"protocol":"vmess","settings":{"vnext":[{"address":"v.example","port":443,
	  "users":[{"id":"uuid-1","alterId":0,"security":"auto"}]}]},
	  "streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"sni.example"},
	  "wsSettings":{"path":"/ws","headers":{"Host":"cdn.example"}}}}`
	link, _, err := BuildLink("vm", ob)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "vmess://") {
		t.Fatalf("link = %q", link)
	}
	name, ob2, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm" {
		t.Errorf("name = %q", name)
	}
	if a, b := mustParseOutbound(t, ob), mustParseOutbound(t, ob2); !reflect.DeepEqual(a, b) {
		t.Errorf("round trip:\n got %v\nwant %v", b, a)
	}
}

// What a link can't hold is reported, not silently dropped.
func TestBuildLinkNotes(t *testing.T) {
	ob := `{"protocol":"vless","mux":{"enabled":true},"settings":{"vnext":[
	  {"address":"a","port":1,"users":[{"id":"u"}]},{"address":"b","port":2,"users":[{"id":"u"}]}]},
	  "streamSettings":{"network":"kcp","sockopt":{"mark":1}}}`
	_, notes, err := BuildLink("x", ob)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(notes, "|")
	for _, want := range []string{"mux", "sockopt", "1 more servers", "kcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes %q missing %q", got, want)
		}
	}
}

func TestBuildLinkRejects(t *testing.T) {
	for _, ob := range []string{
		`not json`,
		`{"settings":{}}`,
		`{"protocol":"freedom"}`,
		`{"protocol":"vless","settings":{"vnext":[]}}`,
	} {
		if _, _, err := BuildLink("x", ob); err == nil {
			t.Errorf("BuildLink(%s) = nil error", ob)
		}
	}
}
