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

// TestGroupForDomain_MicrosoftIsLastResort: the vendor-wide "microsoft" group
// has wildcards (*.microsoft.com, *.azureedge.net, *.windows.net) that overlap
// the narrower product groups. It sits last in KnownGroups so those keep their
// hosts on the dashboard, and only unclaimed Microsoft hosts fall through to it.
func TestGroupForDomain_MicrosoftIsLastResort(t *testing.T) {
	cases := []struct{ host, wantKey string }{
		{"dotnet.microsoft.com", "dotnet"},
		{"dotnetcli.azureedge.net", "dotnet"},
		{"api.githubcopilot.com", "github-copilot"},
		{"github.com", "github"},
		// Not claimed by any narrower group → Microsoft.
		{"login.microsoftonline.com", "microsoft"},
		{"outlook.office365.com", "microsoft"},
		{"contoso.sharepoint.com", "microsoft"},
		{"www.bing.com", "microsoft"},
		{"www.linkedin.com", "microsoft"},
	}
	for _, c := range cases {
		key, _, ok := GroupForDomain(c.host)
		if !ok || key != c.wantKey {
			t.Errorf("GroupForDomain(%q) = (%q, %v), want %q", c.host, key, ok, c.wantKey)
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
