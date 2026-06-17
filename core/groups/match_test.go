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
