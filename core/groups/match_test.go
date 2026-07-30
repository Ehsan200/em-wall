package groups

import "testing"

func TestGroupForDomain(t *testing.T) {
	cases := []struct {
		host    string
		wantKey string
		wantOK  bool
	}{
		{"anthropic.com", "anthropic", true},        // apex matches *.anthropic.com
		{"api.anthropic.com", "anthropic", true},    // subdomain
		{"deep.sub.claude.ai", "anthropic", true},   // multi-label subdomain
		{"chatgpt.com", "openai", true},             // different group
		{"nobody.example.org", "", false},           // unowned
		{"", "", false},                             // empty
	}
	for _, c := range cases {
		key, _, ok := GroupForDomain(c.host)
		if ok != c.wantOK || key != c.wantKey {
			t.Errorf("GroupForDomain(%q) = (%q, %v), want (%q, %v)", c.host, key, ok, c.wantKey, c.wantOK)
		}
	}
}

func TestGroupForKey_IP(t *testing.T) {
	cases := []struct {
		key     string
		wantKey string
		wantOK  bool
	}{
		{"149.154.167.41", "telegram", true}, // inside Telegram DC 149.154.160.0/20
		{"91.108.4.5", "telegram", true},     // inside 91.108.4.0/22
		{"t.me", "telegram", true},           // domain still resolves
		{"8.8.8.8", "", false},               // unowned IP
		{"149.154.176.1", "", false},         // outside the /20 range
	}
	for _, c := range cases {
		key, _, ok := GroupForKey(c.key)
		if ok != c.wantOK || key != c.wantKey {
			t.Errorf("GroupForKey(%q) = (%q, %v), want (%q, %v)", c.key, key, ok, c.wantKey, c.wantOK)
		}
	}
}
