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
		// Hosting surfaces the Labs experiments ship on.
		"stitch-prod.appspot.com",
		"opal-labs.web.app",
		"aistudio-x.firebaseapp.com",
		"lh3.usercontent.goog",
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
		"myapp.appspot.com",
		"myapp.web.app",
		"myapp.firebaseapp.com",
		"myapp-123.cloudfunctions.net",
		"myservice-abc.run.app",
		"lh3.usercontent.goog",
		"blogspot.com",
		"www.waze.com",
	} {
		if !covered(h) {
			t.Errorf("google group must cover %q but does not", h)
		}
	}
}

// TestMicrosoft_CoversVendorSurface: the "microsoft" group is the vendor-wide
// counterpart to "google". The hosts below are the ones that break first when
// only microsoft.com is routed — sign-in (Entra), the M365 workloads on their
// own apexes, and the CDN/asset hosts the apps pull from.
func TestMicrosoft_CoversVendorSurface(t *testing.T) {
	g := FindByKey("microsoft")
	if g == nil {
		t.Fatal("microsoft group missing")
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
		// Identity — losing these signs the user out of every product.
		"login.microsoftonline.com",
		"login.live.com",
		"login.windows.net",
		"logincdn.msftauth.net",
		"aadcdn.msauth.net",
		"autologon.microsoftazuread-sso.com",
		"contoso.onmicrosoft.com",
		// M365 workloads.
		"outlook.office365.com",
		"outlook.cloud.microsoft", // the consolidated .microsoft TLD
		"teams.cloud.microsoft",   // ditto
		"res.static.microsoft",    // ditto
		"x.usercontent.microsoft", // ditto
		"contoso.sharepoint.com",
		"admin.onedrive.com",
		"oneclient.sfx.ms",
		"cdn.odc.officeapps.live.com",
		"www.microsoft365.com",
		"graph.microsoft.com",
		"teams.microsoft.com",
		"contoso.mail.protection.outlook.com",
		"x.svc.ms",
		// Azure + Windows.
		"portal.azure.com",
		"contoso.blob.core.windows.net",
		"contoso.azurewebsites.net",
		"download.windowsupdate.com",
		"www.msftconnecttest.com",
		"assets.msn.com",
		"www.bing.com",
		// Other Microsoft-owned brands.
		"www.linkedin.com",
		"static.licdn.com",
		"xsts.auth.xboxlive.com",
		"session.minecraft.net",
		"code.visualstudio.com",
		"main.vscode-cdn.net",
	} {
		if !covered(h) {
			t.Errorf("microsoft group must cover %q but does not", h)
		}
	}
	// GitHub keeps its own groups; folding it in here would double-create
	// rules for anyone who already applied them.
	for _, h := range []string{
		"github.com",
		"api.githubcopilot.com",
		"www.nuget.org",
	} {
		if covered(h) {
			t.Errorf("microsoft group must NOT cover %q (it has its own group)", h)
		}
	}
}

// TestCategories_CoverEveryGroup: an unlabelled group silently falls into
// "Other" in the UI's quick-add sections, which is how a new group ends up
// filed away from its siblings. Every curated key must be in the table, and
// every category must be one the UI knows how to order.
func TestCategories_CoverEveryGroup(t *testing.T) {
	known := map[string]bool{}
	for _, c := range CategoryOrder() {
		known[c] = true
	}
	for _, g := range KnownGroups() {
		c, ok := categories[g.Key]
		if !ok {
			t.Errorf("group %q has no category — add it to core/groups/category.go", g.Key)
			continue
		}
		if !known[c] {
			t.Errorf("group %q has category %q which is not in CategoryOrder()", g.Key, c)
		}
	}
}

// TestDevRegistries_CoverRealFetchHosts: a package manager is useless if the
// registry host is routed but the artifact CDN it redirects to is not (pip →
// files.pythonhosted.org, go → proxy/sum, npm → registry.npmjs.org). Also locks
// the ordering choice that "maven" — not "android-studio" — owns Maven Central.
func TestDevRegistries_CoverRealFetchHosts(t *testing.T) {
	covers := func(key, host string) bool {
		g := FindByKey(key)
		if g == nil {
			t.Fatalf("group %q missing", key)
		}
		for _, p := range g.Patterns {
			if rules.Match(p, host) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ key, host string }{
		{"python", "pypi.org"},
		{"python", "files.pythonhosted.org"},
		{"python", "www.python.org"},
		{"golang", "proxy.golang.org"},
		{"golang", "sum.golang.org"},
		{"golang", "pkg.go.dev"},
		{"maven", "repo1.maven.org"},
		{"maven", "repo.maven.apache.org"},
		{"maven", "plugins.gradle.org"},
		{"maven", "oss.sonatype.org"},
		{"npm", "registry.npmjs.org"},
		{"npm", "nodejs.org"},
		{"rust", "static.crates.io"},
		{"rust", "index.crates.io"},
		{"rust", "static.rust-lang.org"},
		{"ruby", "rubygems.org"},
		{"dotnet", "api.nuget.org"},
		{"php", "repo.packagist.org"},
		{"hashicorp", "releases.hashicorp.com"},
		{"hashicorp", "registry.terraform.io"},
	} {
		if !covers(c.key, c.host) {
			t.Errorf("group %q must cover %q but does not", c.key, c.host)
		}
	}
	// Attribution: the JVM hosts belong to "maven", which is listed before
	// "android-studio" so GroupForKey resolves them there.
	if k, _, ok := GroupForKey("repo1.maven.org"); !ok || k != "maven" {
		t.Errorf("repo1.maven.org attributed to %q (ok=%v), want \"maven\"", k, ok)
	}
}

// TestConsumerBrands_CoverCDNHosts: each of these services serves its real
// payload from a domain that is not the brand domain — Airbnb photos from
// muscache.com (CNAMEd into airbnb.net), Booking photos/bundles from
// bstatic.com, Pandora audio/art from p-cdn.com — so a group holding only
// the brand domain routes the page but not its content.
func TestConsumerBrands_CoverCDNHosts(t *testing.T) {
	covers := func(key, host string) bool {
		g := FindByKey(key)
		if g == nil {
			t.Fatalf("group %q missing", key)
		}
		for _, p := range g.Patterns {
			if rules.Match(p, host) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ key, host string }{
		{"airbnb", "www.airbnb.com"},
		{"airbnb", "a0.muscache.com"},
		{"airbnb", "muscache.production.global.product.origins.airbnb.net"},
		{"airbnb", "www.airbnb.co.uk"},
		{"airbnb", "abnb.me"},
		{"pandora", "www.pandora.com"},
		{"pandora", "cont-1.p-cdn.com"},
		{"pandora", "mediaserver-cont-dc6-1-v4v6.pandora.com"},
		{"pandora", "pdora.co"},
		{"booking", "www.booking.com"},
		{"booking", "cf.bstatic.com"},
		{"booking", "q-xx.bstatic.com"},
	} {
		if !covers(c.key, c.host) {
			t.Errorf("group %q must cover %q but does not", c.key, c.host)
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
