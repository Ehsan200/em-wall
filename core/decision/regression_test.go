package decision

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

// TestDecisionsUnchangedByGroupMetadata pins the engine's decisions for a
// fixture rule DB. Group suggestions, geosite-sourced custom groups and the
// source_tag column only touch group *definitions*; this guards that none
// of that leaks into routing. If an expectation here changes, routing
// changed — that is never a side effect of the geo/suggest work.
func TestDecisionsUnchangedByGroupMetadata(t *testing.T) {
	ctx := context.Background()
	s, err := rules.Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	fixture := []rules.Rule{
		{Pattern: "*.spotify.com", Action: rules.ActionRoute, Interface: "xrayset:nj", Enabled: true},
		{Pattern: "*.scdn.co", Action: rules.ActionRoute, Interface: "xray:us1,us2", Enabled: true},
		{Pattern: "ads.spotify.com", Action: rules.ActionBlock, Enabled: true},
		{Pattern: "*.doubleclick.net", Action: rules.ActionBlock, Enabled: true},
		{Pattern: "*.openai.com", Action: rules.ActionRoute, Interface: "proxy:work", Enabled: true},
		{Pattern: "safe.openai.com", Action: rules.ActionAllow, Enabled: true},
		{Pattern: "*.off.example", Action: rules.ActionBlock, Enabled: false},
		{Pattern: "*.work.com", Action: rules.ActionRoute, Interface: "utun3", Enabled: true},
		{Pattern: "91.108.4.0/22", Action: rules.ActionRoute, Interface: "xrayset:nj", Enabled: true},
	}
	for _, r := range fixture {
		if _, err := s.Add(ctx, r); err != nil {
			t.Fatalf("add %s: %v", r.Pattern, err)
		}
	}

	type want struct {
		outcome Outcome
		iface   string
		pattern string
	}
	cases := map[string]want{
		"spotify.com":           {OutcomeRoute, "xrayset:nj", "*.spotify.com"},
		"open.spotify.com":      {OutcomeRoute, "xrayset:nj", "*.spotify.com"},
		"ads.spotify.com":       {OutcomeBlock, "", "ads.spotify.com"},
		"i.scdn.co":             {OutcomeRoute, "xray:us1,us2", "*.scdn.co"},
		"ad.doubleclick.net":    {OutcomeBlock, "", "*.doubleclick.net"},
		"api.openai.com":        {OutcomeRoute, "proxy:work", "*.openai.com"},
		"safe.openai.com":       {OutcomeAllow, "", "safe.openai.com"},
		"x.off.example":         {OutcomeAllow, "", ""},
		"git.work.com":          {OutcomeRoute, "utun3", "*.work.com"},
		"unrelated.example.org": {OutcomeAllow, "", ""},
		"spotifycdn.com":        {OutcomeAllow, "", ""},
		"SPOTIFY.COM.":          {OutcomeRoute, "xrayset:nj", "*.spotify.com"},
	}

	check := func(stage string) {
		t.Helper()
		e := New(s)
		if err := e.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		for name, w := range cases {
			got := e.Decide(name)
			if got.Outcome != w.outcome || got.Interface != w.iface || got.Pattern != w.pattern {
				t.Errorf("%s: Decide(%q) = %+v, want %+v", stage, name, got, w)
			}
		}
	}

	check("before")

	// Everything the suggestion feature writes: geosite-sourced custom
	// groups overlapping existing rules, a settings key, and a split that
	// rewrites a custom group definition.
	if _, err := s.AddCustomGroup(ctx, rules.CustomGroup{
		DisplayName: "Spotify", SourceTag: "SPOTIFY",
		Patterns: []string{"*.spotify.com", "*.scdn.co", "*.spotifycdn.com", "91.108.4.0/22"},
	}); err != nil {
		t.Fatal(err)
	}
	mine, err := s.AddCustomGroup(ctx, rules.CustomGroup{DisplayName: "Mine", Patterns: []string{"*.scdn.co", "*.work.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCustomGroup(ctx, rules.CustomGroup{Key: mine.Key, DisplayName: "Mine", Patterns: []string{"*.work.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "suggest_dismissed", `["OPENAI"]`); err != nil {
		t.Fatal(err)
	}

	check("after")
}
