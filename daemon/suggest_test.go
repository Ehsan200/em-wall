package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
)

// fakeSites is a geo.SiteIndex over a fixed tag table.
type fakeSites map[string]geo.Tag

func (f fakeSites) TagsFor(name string) []string {
	var out []string
	for n, t := range f {
		if geo.Broad[n] {
			continue
		}
		for _, p := range t.Patterns {
			if rules.MatchKey(p, name) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

func (f fakeSites) Tag(name string) (geo.Tag, bool) {
	t, ok := f[name]
	return t, ok
}

var testSites = fakeSites{
	"SPOTIFY":  {Name: "SPOTIFY", Patterns: []string{"*.spotify.com", "*.scdn.co", "*.spotifycdn.com"}, Skipped: 1},
	"TELEGRAM": {Name: "TELEGRAM", Patterns: []string{"*.telegram.org", "*.t.me"}, CIDRs: []string{"91.108.4.0/22", "149.154.160.0/20"}},
	"CN":       {Name: "CN", Patterns: []string{"*.baidu.com"}},
	"FOO":      {Name: "FOO", Patterns: []string{"*.foo.example"}},
}

func newSuggestDeps(t *testing.T) *handlerDeps {
	t.Helper()
	s, err := rules.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &handlerDeps{store: s}
}

func feed(sug *suggester, names ...string) {
	for _, n := range names {
		sug.observe(n)
	}
}

func TestSuggesterCountsWindowAndNames(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sug := newSuggester(testSites, nil)
	sug.now = func() time.Time { return now }

	feed(sug, "open.spotify.com", "i.scdn.co.", "OPEN.SPOTIFY.COM", "www.baidu.com", "unrelated.example")
	snap := sug.snapshot()
	if len(snap) != 1 || snap[0].Tag != "SPOTIFY" || snap[0].Hits != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if want := []string{"open.spotify.com", "i.scdn.co"}; !reflect.DeepEqual(snap[0].Names, want) {
		t.Errorf("names = %v, want %v", snap[0].Names, want)
	}

	for i := 0; i < 20; i++ {
		sug.observe("h" + string(rune('a'+i)) + ".spotify.com")
	}
	if n := len(sug.snapshot()[0].Names); n != suggestSeenNames {
		t.Errorf("names cap = %d", n)
	}

	// Eight days later nothing is left in the window.
	now = now.Add(8 * 24 * time.Hour)
	if snap := sug.snapshot(); len(snap) != 0 {
		t.Errorf("after window: %+v", snap)
	}
	sug.prune()
	if len(sug.tags) != 0 {
		t.Errorf("prune left %d tags", len(sug.tags))
	}
}

func TestSuggesterOfferNeverBlocks(t *testing.T) {
	sug := newSuggester(testSites, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < suggestQueue*3; i++ {
			sug.Offer("open.spotify.com")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Offer blocked with nobody draining")
	}
	var nilSug *suggester
	nilSug.Offer("x.com") // nil-safe
	sug.enabled.Store(false)
	before := len(sug.ch)
	sug.Offer("open.spotify.com")
	if len(sug.ch) != before {
		t.Error("disabled suggester queued a name")
	}
}

func TestSuggesterHonoursEnabledSetting(t *testing.T) {
	ctx := context.Background()
	d := newSuggestDeps(t)
	sug := newSuggester(testSites, d.store)
	feed(sug, "a.spotify.com")
	_ = d.store.SetSetting(ctx, settingSuggestEnabled, "false")
	sug.refreshEnabled(ctx)
	if sug.enabled.Load() || len(sug.snapshot()) != 0 {
		t.Fatal("disable should stop and forget")
	}
	_ = d.store.SetSetting(ctx, settingSuggestEnabled, "true")
	sug.refreshEnabled(ctx)
	if !sug.enabled.Load() {
		t.Fatal("re-enable")
	}
}

func TestStoreLogSinkUnmatchedFeedsSuggester(t *testing.T) {
	sug := newSuggester(testSites, nil)
	sink := &storeLogSink{ch: make(chan rules.LogEntry, 1), suggest: sug}
	sink.Unmatched("open.spotify.com")
	if len(sug.ch) != 1 {
		t.Fatal("name not offered")
	}
	(&storeLogSink{}).Unmatched("x.com") // no suggester: no panic
}

func TestSuggestListFilters(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	d := newSuggestDeps(t)
	sug := newSuggester(testSites, d.store)
	feed(sug,
		"a.spotify.com", "b.spotify.com", "i.scdn.co",
		"core.telegram.org", "core.telegram.org", "t.me",
		"x.foo.example", "y.foo.example", "z.foo.example",
		"www.baidu.com", "www.baidu.com", "www.baidu.com",
	)

	list, err := d.suggestList(ctx, testSites, sug, now)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]ipc.SuggestionDTO{}
	for _, s := range list {
		tags[s.Tag] = s
	}
	if _, ok := tags["CN"]; ok {
		t.Error("broad tag suggested")
	}
	if len(tags) != 3 {
		t.Fatalf("tags = %v", list)
	}
	if sp := tags["SPOTIFY"]; sp.DisplayName != "Spotify" || sp.Hits != 3 || len(sp.Missing) != 3 || sp.Skipped != 1 {
		t.Errorf("spotify = %+v", sp)
	}

	// A rule covering part of the tag: only the rest is Missing.
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "*.spotify.com", Action: rules.ActionRoute, Interface: "xrayset:nj", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Fully covered by rules → dropped.
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "*.foo.example", Action: rules.ActionBlock, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_ = d.suggestDismiss(ctx, ipc.SuggestDismissParams{Tag: "telegram", Forever: false}, now)

	list, _ = d.suggestList(ctx, testSites, sug, now)
	if len(list) != 1 || list[0].Tag != "SPOTIFY" {
		t.Fatalf("after rules+snooze: %+v", list)
	}
	if want := []string{"*.scdn.co", "*.spotifycdn.com"}; !reflect.DeepEqual(list[0].Missing, want) {
		t.Errorf("missing = %v", list[0].Missing)
	}

	// Snooze runs out after 7 days.
	later, _ := d.suggestList(ctx, testSites, sug, now.Add(suggestSnooze+time.Hour))
	if len(later) != 2 {
		t.Errorf("after snooze expiry: %d", len(later))
	}

	// Never → gone, and listed as dismissed; undo brings it back.
	_ = d.suggestDismiss(ctx, ipc.SuggestDismissParams{Tag: "SPOTIFY", Forever: true}, now)
	if list, _ = d.suggestList(ctx, testSites, sug, now); len(list) != 0 {
		t.Fatalf("after never: %+v", list)
	}
	if p := loadSuggestPrefs(ctx, d.store); !p.dismissed["SPOTIFY"] {
		t.Error("not recorded as dismissed")
	}
	_ = d.suggestUndismiss(ctx, "spotify")
	if list, _ = d.suggestList(ctx, testSites, sug, now); len(list) != 1 {
		t.Fatalf("after undo: %+v", list)
	}

	// Turned off → nothing.
	_ = d.store.SetSetting(ctx, settingSuggestEnabled, "false")
	if list, _ = d.suggestList(ctx, testSites, sug, now); len(list) != 0 {
		t.Error("suggestions while disabled")
	}
}

func TestSuggestCollisions(t *testing.T) {
	ctx := context.Background()
	d := newSuggestDeps(t)
	mine, err := d.store.AddCustomGroup(ctx, rules.CustomGroup{DisplayName: "Music", Patterns: []string{"*.scdn.co", "open.spotify.com", "*.deezer.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "*.scdn.co", Action: rules.ActionRoute, Interface: "xrayset:nj", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sug := newSuggester(testSites, d.store)
	feed(sug, "a.spotify.com", "a.spotify.com", "a.spotify.com")
	list, _ := d.suggestList(ctx, testSites, sug, time.Now())
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	var custom *ipc.SuggestCollisionDTO
	for i, c := range list[0].Collisions {
		if c.GroupKey == mine.Key {
			custom = &list[0].Collisions[i]
		}
	}
	if custom == nil {
		t.Fatalf("no collision with custom group: %+v", list[0].Collisions)
	}
	if !custom.Custom || !reflect.DeepEqual(custom.Patterns, []string{"*.scdn.co", "open.spotify.com"}) ||
		!reflect.DeepEqual(custom.Interfaces, []string{"xrayset:nj"}) || !reflect.DeepEqual(custom.Actions, []string{"route"}) {
		t.Errorf("collision = %+v", *custom)
	}
	// The curated Spotify group overlaps too.
	found := false
	for _, c := range list[0].Collisions {
		if c.GroupKey == "spotify" && !c.Custom {
			found = true
		}
	}
	if !found {
		t.Error("curated spotify collision missing")
	}
}

func TestSuggestAcceptCreatesGroupOnly(t *testing.T) {
	ctx := context.Background()
	d := newSuggestDeps(t)
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "*.telegram.org", Action: rules.ActionBlock, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before, _ := d.store.List(ctx)

	key, err := d.suggestAccept(ctx, testSites, ipc.SuggestAcceptParams{Tag: "telegram", IncludeIPs: true})
	if err != nil {
		t.Fatal(err)
	}
	g, err := d.store.GetCustomGroup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if g.SourceTag != "TELEGRAM" || g.DisplayName != "Telegram" {
		t.Errorf("group = %+v", g)
	}
	if want := []string{"*.t.me", "91.108.4.0/22", "149.154.160.0/20"}; !reflect.DeepEqual(g.Patterns, want) {
		t.Errorf("patterns = %v, want %v (Missing + CIDRs)", g.Patterns, want)
	}
	after, _ := d.store.List(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Error("accept changed rule rows")
	}

	// A second accept of the same tag gets a fresh key.
	key2, err := d.suggestAccept(ctx, testSites, ipc.SuggestAcceptParams{Tag: "TELEGRAM", Patterns: []string{"*.telegram.org"}})
	if err != nil || key2 == key || !strings.HasPrefix(key2, key) {
		t.Errorf("second accept key = %q, %v", key2, err)
	}

	// Too many IPs is refused rather than silently truncated.
	many := geo.Tag{Name: "BIG", Patterns: []string{"*.big.example"}}
	for i := 0; i < suggestMaxIPs+1; i++ {
		many.CIDRs = append(many.CIDRs, "10.0.0.0/8")
	}
	if _, err := d.suggestAccept(ctx, fakeSites{"BIG": many}, ipc.SuggestAcceptParams{Tag: "BIG", IncludeIPs: true}); err == nil {
		t.Error("expected too-many-IPs error")
	}

	// The accepted tag stops being suggested.
	sug := newSuggester(testSites, d.store)
	feed(sug, "t.me", "t.me", "t.me")
	if list, _ := d.suggestList(ctx, testSites, sug, time.Now()); len(list) != 0 {
		t.Errorf("accepted tag still suggested: %+v", list)
	}
}

func TestSuggestSplitMovesDefinitionsOnly(t *testing.T) {
	ctx := context.Background()
	d := newSuggestDeps(t)
	a, _ := d.store.AddCustomGroup(ctx, rules.CustomGroup{DisplayName: "Music", Patterns: []string{"*.scdn.co", "*.deezer.com"}, Color: "#111111"})
	b, _ := d.store.AddCustomGroup(ctx, rules.CustomGroup{DisplayName: "Work", Description: "w", Patterns: []string{"open.spotify.com", "*.corp.example"}})
	if _, err := d.store.Add(ctx, rules.Rule{Pattern: "*.scdn.co", Action: rules.ActionRoute, Interface: "xrayset:nj", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rulesBefore, _ := d.store.List(ctx)

	res, err := d.suggestSplit(ctx, testSites, ipc.SuggestSplitParams{
		Tag:        "SPOTIFY",
		Patterns:   []string{"*.scdn.co", "open.spotify.com", "*.deezer.com"}, // deezer isn't in the tag: must stay
		FromGroups: []string{a.Key, b.Key},
	})
	if err != nil {
		t.Fatal(err)
	}
	ng, _ := d.store.GetCustomGroup(ctx, res.NewGroupKey)
	if !reflect.DeepEqual(ng.Patterns, []string{"*.scdn.co", "open.spotify.com"}) || ng.SourceTag != "SPOTIFY" {
		t.Errorf("new group = %+v", ng)
	}
	ga, _ := d.store.GetCustomGroup(ctx, a.Key)
	gb, _ := d.store.GetCustomGroup(ctx, b.Key)
	if !reflect.DeepEqual(ga.Patterns, []string{"*.deezer.com"}) || ga.Color != "#111111" ||
		!reflect.DeepEqual(gb.Patterns, []string{"*.corp.example"}) || gb.Description != "w" {
		t.Errorf("after split: a=%+v b=%+v", ga, gb)
	}
	rulesAfter, _ := d.store.List(ctx)
	if !reflect.DeepEqual(rulesBefore, rulesAfter) {
		t.Error("split changed rule rows")
	}

	// Undo = replay Before through groups.update + delete the new group.
	if len(res.Before) != 2 {
		t.Fatalf("before = %+v", res.Before)
	}
	for _, p := range res.Before {
		if err := d.store.UpdateCustomGroup(ctx, rules.CustomGroup{Key: p.Key, DisplayName: p.DisplayName, Description: p.Description, Patterns: p.Patterns, Color: p.Color}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.store.DeleteCustomGroup(ctx, res.NewGroupKey)
	ga, _ = d.store.GetCustomGroup(ctx, a.Key)
	if !reflect.DeepEqual(ga.Patterns, a.Patterns) {
		t.Errorf("undo: %+v", ga)
	}

	// Curated groups are never edited.
	if _, err := d.suggestSplit(ctx, testSites, ipc.SuggestSplitParams{Tag: "SPOTIFY", Patterns: []string{"*.spotify.com"}, FromGroups: []string{"spotify"}}); err == nil {
		t.Error("split of a curated group must fail")
	}
}

func TestGeositeGroupDriftAndSyncTag(t *testing.T) {
	ctx := context.Background()
	d := newSuggestDeps(t)
	key, err := d.suggestAccept(ctx, testSites, ipc.SuggestAcceptParams{Tag: "SPOTIFY"})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := d.store.GetCustomGroup(ctx, key)
	stored, _ := d.store.List(ctx)

	var dto ipc.GroupDTO
	fillTagDrift(&dto, g, testSites, stored)
	if dto.SourceTag != "SPOTIFY" || len(dto.TagNew) != 0 || len(dto.TagGone) != 0 {
		t.Fatalf("fresh group drift: %+v", dto)
	}

	// A newer geosite.dat adds one domain and drops another.
	newer := fakeSites{"SPOTIFY": {Name: "SPOTIFY", Patterns: []string{"*.spotify.com", "*.scdn.co", "*.spotify.design"}}}
	dto = ipc.GroupDTO{}
	fillTagDrift(&dto, g, newer, stored)
	if !reflect.DeepEqual(dto.TagNew, []string{"*.spotify.design"}) || !reflect.DeepEqual(dto.TagGone, []string{"*.spotifycdn.com"}) {
		t.Fatalf("drift = new %v gone %v", dto.TagNew, dto.TagGone)
	}
	// No geo data → nothing claimed.
	dto = ipc.GroupDTO{}
	fillTagDrift(&dto, g, nil, stored)
	if dto.SourceTag != "SPOTIFY" || dto.TagNew != nil || dto.TagGone != nil {
		t.Errorf("without geo: %+v", dto)
	}

	if err := d.groupsSyncTag(ctx, newer, key); err != nil {
		t.Fatal(err)
	}
	g, _ = d.store.GetCustomGroup(ctx, key)
	if want := []string{"*.spotify.com", "*.scdn.co", "*.spotifycdn.com", "*.spotify.design"}; !reflect.DeepEqual(g.Patterns, want) {
		t.Errorf("after sync-tag = %v (gone ones are kept)", g.Patterns)
	}
	if g.SourceTag != "SPOTIFY" {
		t.Error("sync-tag lost source tag")
	}
	if after, _ := d.store.List(ctx); len(after) != len(stored) {
		t.Error("sync-tag created rules")
	}

	plain, _ := d.store.AddCustomGroup(ctx, rules.CustomGroup{DisplayName: "Plain", Patterns: []string{"*.x.com"}})
	if err := d.groupsSyncTag(ctx, newer, plain.Key); err == nil {
		t.Error("sync-tag on a non-geosite group must fail")
	}
}

func TestTagDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"SPOTIFY":         "Spotify",
		"spotify":         "Spotify",
		"TWITTER":         "X (Twitter)",
		"APPLE-CN":        "Apple CN",
		"CATEGORY-GAMES":  "Games (category)",
		"CATEGORY-IR":     "IR (category)",
		"GEOLOCATION-!CN": "Geolocation !CN",
		"GOOGLE-PLAY":     "Google Play",
	} {
		if got := tagDisplayName(in); got != want {
			t.Errorf("tagDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPushFront(t *testing.T) {
	l := pushFront(nil, "a", 3)
	l = pushFront(l, "b", 3)
	l = pushFront(l, "c", 3)
	l = pushFront(l, "a", 3)
	l = pushFront(l, "d", 3)
	if !reflect.DeepEqual(l, []string{"d", "a", "c"}) {
		t.Errorf("pushFront = %v", l)
	}
}
