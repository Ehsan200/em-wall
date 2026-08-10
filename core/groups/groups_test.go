package groups

import (
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

func TestKnownGroups_HaveExpectedKeys(t *testing.T) {
	want := []string{"anthropic", "openai", "google-ai"}
	have := map[string]bool{}
	for _, g := range KnownGroups() {
		have[g.Key] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing group %q", w)
		}
	}
}

func TestKnownGroups_PatternsLookValid(t *testing.T) {
	for _, g := range KnownGroups() {
		if g.Key == "" || g.DisplayName == "" {
			t.Errorf("group has empty key/name: %+v", g)
		}
		if len(g.Patterns) == 0 {
			t.Errorf("group %q has no patterns", g.Key)
		}
		for _, p := range g.Patterns {
			if p == "" {
				t.Errorf("group %q has an empty pattern", g.Key)
			}
		}
	}
}

func TestFindByKey(t *testing.T) {
	g := FindByKey("anthropic")
	if g == nil {
		t.Fatal("expected to find anthropic")
	}
	if g.Key != "anthropic" {
		t.Errorf("got %q", g.Key)
	}
	if FindByKey("nonexistent") != nil {
		t.Errorf("expected nil for unknown key")
	}
}

// TestGoogleAI_CoversGeminiBackends locks in the fix for the real-IP leak:
// Gemini/AI Studio call shared Google "-pa" backend hosts (signaler-pa,
// ogads-pa, alkalimakersuite-pa, …) that were not in the group, so they left
// on the default route and Google answered 403. They must now be covered —
// while the rest of Google (Search/Gmail/Drive) must stay OUT of the group so
// enabling it does not route all of Google through the proxy.
func TestGoogleAI_CoversGeminiBackends(t *testing.T) {
	g := FindByKey("google-ai")
	if g == nil {
		t.Fatal("google-ai group missing")
	}
	covered := func(host string) bool {
		for _, p := range g.Patterns {
			if rules.Match(p, host) {
				return true
			}
		}
		return false
	}
	mustCover := []string{
		"signaler-pa.clients6.google.com",         // the reported 403
		"ogads-pa.clients6.google.com",            // the reported 403
		"alkalimakersuite-pa.clients6.google.com", // AI Studio backend
		"gemini.google.com",
		"generativelanguage.googleapis.com",
		"proactivebackend-pa.googleapis.com",
		// Labs / non-google.com product surfaces. Any experiment on
		// withgoogle.com or on the .google TLD must be covered without
		// naming the product.
		"stitch.withgoogle.com",
		"aitestkitchen.withgoogle.com",
		"opal.withgoogle.com",
		"labs.google",
		"jules.google",
		"notebooklm.google",
		"flow.google",
		"opal.google",
		"deepmind.google",
		"illuminate.google.com",
		// Shared app-shell / auth backends they all call.
		"www.gstatic.com",
		"accounts.google.com",
		"oauth2.googleapis.com",
		"identitytoolkit.googleapis.com",
		"aisandbox-pa.googleapis.com",
	}
	for _, h := range mustCover {
		if !covered(h) {
			t.Errorf("google-ai must cover %q (Gemini backend) but does not", h)
		}
	}
	mustNotCover := []string{
		"www.google.com",   // Search
		"mail.google.com",  // Gmail
		"drive.google.com", // Drive
		"maps.googleapis.com",
	}
	for _, h := range mustNotCover {
		if covered(h) {
			t.Errorf("google-ai must NOT cover %q (would route all of Google)", h)
		}
	}
}

// TestGoogle_CoversAllOfGoogle: the broad "google" group is the opposite of
// google-ai — it MUST cover the mainstream Google products that google-ai
// deliberately excludes.
func TestGoogle_CoversAllOfGoogle(t *testing.T) {
	g := FindByKey("google")
	if g == nil {
		t.Fatal("google group missing")
	}
	covered := func(host string) bool {
		for _, p := range g.Patterns {
			if rules.Match(p, host) {
				return true
			}
		}
		return false
	}
	for _, h := range []string{
		"www.google.com",
		"mail.google.com",
		"drive.google.com",
		"maps.googleapis.com",
		"photos.google.com",
		"accounts.google.com",
		"lh3.googleusercontent.com",
		"fonts.gstatic.com",
	} {
		if !covered(h) {
			t.Errorf("google group must cover %q but does not", h)
		}
	}
}

// TestGrok_CoversUserContentAndXSurface locks in the login-via-X gap: Grok
// serves user content from grokusercontent.com and is reachable as grok.x.com
// during "sign in with X", neither covered by the original three patterns.
func TestGrok_CoversUserContentAndXSurface(t *testing.T) {
	g := FindByKey("grok")
	if g == nil {
		t.Fatal("grok group missing")
	}
	covered := func(host string) bool {
		for _, p := range g.Patterns {
			if rules.Match(p, host) {
				return true
			}
		}
		return false
	}
	for _, h := range []string{
		"grokusercontent.com",
		"assets.grokusercontent.com",
		"grok.x.com",
		"api.x.ai",
	} {
		if !covered(h) {
			t.Errorf("grok group must cover %q but does not", h)
		}
	}
}
