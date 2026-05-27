package proxy

import "testing"

func TestValidProtocol(t *testing.T) {
	for _, p := range []Protocol{ProtocolSOCKS5, ProtocolHTTP} {
		if !ValidProtocol(p) {
			t.Errorf("ValidProtocol(%q) = false, want true", p)
		}
	}
	for _, p := range []Protocol{"", "https", "shadowsocks", "SOCKS5"} {
		if ValidProtocol(p) {
			t.Errorf("ValidProtocol(%q) = true, want false", p)
		}
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"work", true},
		{"my-proxy", true},
		{"home_2", true},
		{"a", true},
		{"", false},
		{"  ", false},
		{"Has Space", false},
		{"UPPER", true}, // normalized to lower in ValidName
		{"weird!", false},
		{"with.dot", false},
	}
	for _, c := range cases {
		if got := ValidName(c.name); got != c.want {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsProxyInterface(t *testing.T) {
	cases := map[string]bool{
		"":                false,
		"utun3":           false,
		"app:tailscale":   false,
		"proxy:":          true, // prefix matches even if empty body — caller catches via ParseInterface
		"proxy:work":      true,
		"proxy:work,home": true,
	}
	for in, want := range cases {
		if got := IsProxyInterface(in); got != want {
			t.Errorf("IsProxyInterface(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseInterface(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"proxy:work", []string{"work"}},
		{"proxy:work,home", []string{"work", "home"}},
		{"proxy: work , home ", []string{"work", "home"}},
		{"proxy:,,", nil}, // empty entries dropped
		{"proxy:", nil},
		{"utun3", nil},
		{"app:tailscale", nil},
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
