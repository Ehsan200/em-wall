package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/selfuse"
)

// Group suggestions over IPC. Contract: docs/geo-map-suggest.md §2.
//
// Invariant: nothing here writes a rule row. accept and split create or
// edit custom group *definitions* only; routing changes only when the user
// then applies a group through the normal groups.apply flow.

const (
	suggestSnooze    = 7 * 24 * time.Hour // "Not now"
	suggestMaxIPs    = 64                 // CIDRs accept adds on its own
	suggestMaxListed = 12                 // cards in one suggest.list
)

// suggestPrefsMu serialises read-modify-write of the dismissed/snoozed
// settings (two quick clicks must not lose one).
var suggestPrefsMu sync.Mutex

func registerSuggestHandlers(s *ipc.Server, d *handlerDeps) {
	// main installs the suggester before registering handlers and starts
	// its run loop after, so this is the place to hand it the stores for
	// em-wall's own hostnames without more wiring there.
	if sug := activeSuggester.Load(); sug != nil && sug.selfLoad == nil {
		sug.selfLoad = d.loadSelfHosts
	}
	s.Handle(ipc.MethodSuggestList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.suggestList(ctx, d.siteIndex(), activeSuggester.Load(), time.Now())
	})

	s.Handle(ipc.MethodSuggestDismiss, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SuggestDismissParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return nil, d.suggestDismiss(ctx, p, time.Now())
	})

	s.Handle(ipc.MethodSuggestUndismiss, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SuggestTagParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return nil, d.suggestUndismiss(ctx, p.Tag)
	})

	s.Handle(ipc.MethodSuggestDismissed, func(ctx context.Context, _ json.RawMessage) (any, error) {
		prefs := loadSuggestPrefs(ctx, d.store)
		out := make([]string, 0, len(prefs.dismissed))
		for t := range prefs.dismissed {
			out = append(out, t)
		}
		sort.Strings(out)
		return out, nil
	})

	s.Handle(ipc.MethodSuggestAccept, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SuggestAcceptParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		key, err := d.suggestAccept(ctx, d.siteIndex(), p)
		if err == nil {
			activeSuggester.Load().forget(normTag(p.Tag))
		}
		return key, err
	})

	s.Handle(ipc.MethodSuggestSplit, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SuggestSplitParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		res, err := d.suggestSplit(ctx, d.siteIndex(), p)
		if err == nil {
			activeSuggester.Load().forget(normTag(p.Tag))
		}
		return res, err
	})

	s.Handle(ipc.MethodGroupsSyncTag, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsSyncTagParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return nil, d.groupsSyncTag(ctx, d.siteIndex(), p.Key)
	})
}

// siteIndex returns the geosite index, or nil (not a typed nil) when the
// geo DB isn't wired.
func (d *handlerDeps) siteIndex() geo.SiteIndex {
	if d.geoDB == nil {
		return nil
	}
	return d.geoDB
}

func normTag(t string) string { return strings.ToUpper(strings.TrimSpace(t)) }

// ---- prefs: dismissed ("never") and snoozed ("not now") ----

type suggestPrefs struct {
	dismissed map[string]bool
	snoozed   map[string]time.Time
}

func loadSuggestPrefs(ctx context.Context, store settingsReader) suggestPrefs {
	p := suggestPrefs{dismissed: map[string]bool{}, snoozed: map[string]time.Time{}}
	if v, _ := store.GetSetting(ctx, settingSuggestDismissed, ""); v != "" {
		var list []string
		if json.Unmarshal([]byte(v), &list) == nil {
			for _, t := range list {
				if t = normTag(t); t != "" {
					p.dismissed[t] = true
				}
			}
		}
	}
	if v, _ := store.GetSetting(ctx, settingSuggestSnoozed, ""); v != "" {
		var m map[string]string
		if json.Unmarshal([]byte(v), &m) == nil {
			for t, until := range m {
				if ts, err := time.Parse(time.RFC3339, until); err == nil {
					p.snoozed[normTag(t)] = ts
				}
			}
		}
	}
	return p
}

// save writes both keys, dropping snoozes that have run out.
func (p suggestPrefs) save(ctx context.Context, store *rules.Store, now time.Time) error {
	list := make([]string, 0, len(p.dismissed))
	for t := range p.dismissed {
		list = append(list, t)
	}
	sort.Strings(list)
	b, _ := json.Marshal(list)
	if err := store.SetSetting(ctx, settingSuggestDismissed, string(b)); err != nil {
		return err
	}
	m := map[string]string{}
	for t, until := range p.snoozed {
		if until.After(now) {
			m[t] = until.UTC().Format(time.RFC3339)
		}
	}
	b, _ = json.Marshal(m)
	return store.SetSetting(ctx, settingSuggestSnoozed, string(b))
}

func (d *handlerDeps) suggestDismiss(ctx context.Context, p ipc.SuggestDismissParams, now time.Time) error {
	tag := normTag(p.Tag)
	if tag == "" {
		return errors.New("tag is required")
	}
	suggestPrefsMu.Lock()
	defer suggestPrefsMu.Unlock()
	prefs := loadSuggestPrefs(ctx, d.store)
	if p.Forever {
		prefs.dismissed[tag] = true
		delete(prefs.snoozed, tag)
	} else {
		prefs.snoozed[tag] = now.Add(suggestSnooze)
	}
	return prefs.save(ctx, d.store, now)
}

func (d *handlerDeps) suggestUndismiss(ctx context.Context, tag string) error {
	tag = normTag(tag)
	suggestPrefsMu.Lock()
	defer suggestPrefsMu.Unlock()
	prefs := loadSuggestPrefs(ctx, d.store)
	delete(prefs.dismissed, tag)
	delete(prefs.snoozed, tag)
	return prefs.save(ctx, d.store, time.Now())
}

// ---- list ----

// suggestGroup is one existing group as the suggester sees it.
type suggestGroup struct {
	key, name string
	custom    bool
	sourceTag string
	patterns  []string
}

func (d *handlerDeps) suggestGroups(ctx context.Context) ([]suggestGroup, error) {
	var out []suggestGroup
	for _, g := range groups.KnownGroups() {
		out = append(out, suggestGroup{key: g.Key, name: g.DisplayName, patterns: d.effectiveGroupPatterns(ctx, g)})
	}
	custom, err := d.store.ListCustomGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range custom {
		out = append(out, suggestGroup{key: g.Key, name: g.DisplayName, custom: true, sourceTag: g.SourceTag, patterns: g.Patterns})
	}
	return out, nil
}

func (d *handlerDeps) suggestList(ctx context.Context, idx geo.SiteIndex, sug *suggester, now time.Time) ([]ipc.SuggestionDTO, error) {
	out := []ipc.SuggestionDTO{}
	if idx == nil || sug == nil {
		return out, nil
	}
	if v, _ := d.store.GetSetting(ctx, settingSuggestEnabled, "true"); v == "false" {
		return out, nil
	}
	stats := sug.snapshot()
	if len(stats) == 0 {
		return out, nil
	}
	stored, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	grps, err := d.suggestGroups(ctx)
	if err != nil {
		return nil, err
	}
	return buildSuggestions(stats, idx, stored, grps, loadSuggestPrefs(ctx, d.store), sug.selfSet(), now), nil
}

// buildSuggestions turns the seen-tag window into suggestions: drops what
// is not suggestible, dismissed, snoozed, too rare, already a group, fully
// covered by rules, or seen only under names a rule now covers, and works
// out each one's Missing part, built-in alternative, collisions and the
// em-wall hostnames it covers.
func buildSuggestions(stats []tagStat, idx geo.SiteIndex, stored []rules.Rule, grps []suggestGroup, prefs suggestPrefs, self selfuse.Set, now time.Time) []ipc.SuggestionDTO {
	owned := map[string]bool{}
	for _, g := range grps {
		if g.sourceTag != "" {
			owned[g.sourceTag] = true
		}
	}
	out := []ipc.SuggestionDTO{}
	for _, st := range stats {
		tag := st.Tag
		if !geo.Suggestible(tag) || prefs.dismissed[tag] || owned[tag] ||
			st.Hits < suggestMinHits || st.LastSeen.Sub(st.FirstSeen) < suggestMinSpan {
			continue
		}
		if until, ok := prefs.snoozed[tag]; ok && until.After(now) {
			continue
		}
		t, ok := idx.Tag(tag)
		if !ok {
			continue
		}
		pats := tagPatterns(t)
		if len(pats) == 0 {
			continue
		}
		var missing []string
		for _, p := range pats {
			if !groupPatternApplied(p, stored) {
				missing = append(missing, p)
			}
		}
		if len(missing) == 0 {
			continue
		}
		// Names counted before a rule (or an applied built-in) took them
		// over no longer argue for the tag.
		seen := uncoveredNames(st.Names, stored)
		if len(seen) == 0 {
			continue
		}
		builtin := builtinFor(tag, seen, grps, stored)
		dto := ipc.SuggestionDTO{
			Tag:         tag,
			DisplayName: tagDisplayName(tag),
			Patterns:    pats,
			CIDRs:       nonNil(t.CIDRs),
			Skipped:     t.Skipped,
			SeenNames:   seen,
			Hits:        st.Hits,
			LastSeen:    st.LastSeen.UTC().Format(time.RFC3339),
			Missing:     missing,
			Collisions:  tagCollisions(pats, stored, grps),
			UsedBy:      selfUsesIn(pats, self),
		}
		if builtin != nil {
			dto.Builtin, dto.BuiltinName = builtin.key, builtin.name
		}
		out = append(out, dto)
		if len(out) == suggestMaxListed {
			break
		}
	}
	return out
}

// uncoveredNames keeps the names no enabled rule matches.
func uncoveredNames(names []string, stored []rules.Rule) []string {
	out := []string{}
	for _, n := range names {
		covered := false
		for _, r := range stored {
			if r.Enabled && rules.MatchKey(r.Pattern, n) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, n)
		}
	}
	return out
}

// builtinFor picks the curated group to apply instead of creating a custom
// one: the one named like the tag, else the one covering most (over half)
// of the seen names. A curated group whose every pattern already has rules
// has nothing left to apply.
func builtinFor(tag string, seen []string, grps []suggestGroup, stored []rules.Rule) *suggestGroup {
	named := curatedKeyForTag(tag)
	var best *suggestGroup
	bestN := 0
	for i := range grps {
		g := &grps[i]
		if g.custom || len(g.patterns) == 0 {
			continue
		}
		n := 0
		for _, name := range seen {
			for _, p := range g.patterns {
				if rules.MatchKey(p, name) {
					n++
					break
				}
			}
		}
		isNamed := g.key == named
		if !isNamed && n*2 <= len(seen) {
			continue
		}
		if !groupHasUnapplied(g.patterns, stored) {
			continue
		}
		if isNamed {
			return g
		}
		if n > bestN {
			best, bestN = g, n
		}
	}
	return best
}

func groupHasUnapplied(pats []string, stored []rules.Rule) bool {
	for _, p := range pats {
		if !groupPatternApplied(p, stored) {
			return true
		}
	}
	return false
}

// selfUsesIn lists em-wall's own hostnames the patterns cover, by host.
// Patterns go in a set so each host costs one lookup per label rather
// than a match against every pattern (big tags list thousands).
// usedByOrNil is selfUsesIn for groups.list: nil when nothing matches, so
// the field drops out of the JSON for the common case.
func usedByOrNil(pats []string, self selfuse.Set) []ipc.SelfUseDTO {
	if u := selfUsesIn(pats, self); len(u) > 0 {
		return u
	}
	return nil
}

func selfUsesIn(pats []string, self selfuse.Set) []ipc.SelfUseDTO {
	out := []ipc.SelfUseDTO{}
	if len(self) == 0 {
		return out
	}
	set := make(map[string]bool, len(pats))
	for _, p := range pats {
		set[p] = true
	}
	hosts := make([]string, 0, len(self))
	for h := range self {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		hit := set[h]
		for s := h; !hit; {
			if set["*."+s] {
				hit = true
				break
			}
			dot := strings.IndexByte(s, '.')
			if dot < 0 {
				break
			}
			s = s[dot+1:]
		}
		if !hit {
			continue
		}
		for _, purpose := range self.Purposes(h) {
			out = append(out, ipc.SelfUseDTO{Host: h, Purpose: purpose})
		}
	}
	return out
}

// tagPatterns returns a tag's patterns em-wall can store, normalised.
func tagPatterns(t geo.Tag) []string {
	out := make([]string, 0, len(t.Patterns))
	seen := map[string]bool{}
	for _, p := range t.Patterns {
		p = normalizeGroupPattern(p)
		// Never take a CDN/cloud edge domain into a group: it would route
		// every other site that network serves (geo.EdgeDomains).
		if p == "" || seen[p] || rules.Validate(p) != nil || geo.IsEdgePattern(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// insideTag reports whether a group pattern lies within the tag (equal to
// or narrower than one of its patterns) — the part a split may move.
func insideTag(groupPat string, tagPats []string) bool {
	for _, tp := range tagPats {
		if ruleCoveredByGroupPattern(groupPat, tp) {
			return true
		}
	}
	return false
}

// tagCollisions lists existing groups with patterns inside the tag whose
// rules route that overlap, and those rules' actions/interfaces.
func tagCollisions(tagPats []string, stored []rules.Rule, grps []suggestGroup) []ipc.SuggestCollisionDTO {
	out := []ipc.SuggestCollisionDTO{}
	for _, g := range grps {
		var overlap []string
		for _, gp := range g.patterns {
			if insideTag(gp, tagPats) {
				overlap = append(overlap, gp)
			}
		}
		if len(overlap) == 0 {
			continue
		}
		ifaces, actions := map[string]bool{}, map[string]bool{}
		for _, r := range stored {
			if ruleBelongsToGroup(r.Pattern, overlap) {
				ifaces[r.Interface] = true
				actions[string(r.Action)] = true
			}
		}
		// Only overlap that existing rules route is a collision: a group
		// that was never applied decides nothing yet.
		if len(actions) == 0 {
			continue
		}
		out = append(out, ipc.SuggestCollisionDTO{
			GroupKey:   g.key,
			GroupName:  g.name,
			Custom:     g.custom,
			Patterns:   overlap,
			Interfaces: sortedKeys(ifaces),
			Actions:    sortedKeys(actions),
		})
	}
	return out
}

// ---- accept / split / sync-tag ----

func (d *handlerDeps) suggestAccept(ctx context.Context, idx geo.SiteIndex, p ipc.SuggestAcceptParams) (string, error) {
	tag := normTag(p.Tag)
	if idx == nil {
		return "", errors.New("geosite data is not loaded")
	}
	t, ok := idx.Tag(tag)
	if !ok {
		return "", fmt.Errorf("unknown geosite tag %q", tag)
	}
	pats := p.Patterns
	if len(pats) == 0 {
		stored, err := d.store.List(ctx)
		if err != nil {
			return "", err
		}
		all := tagPatterns(t)
		for _, tp := range all {
			if !groupPatternApplied(tp, stored) {
				pats = append(pats, tp)
			}
		}
		if len(pats) == 0 {
			pats = all
		}
	}
	if p.IncludeIPs && len(t.CIDRs) > 0 {
		if len(t.CIDRs) > suggestMaxIPs {
			return "", fmt.Errorf("%s lists %d IP ranges (more than %d); pass them as patterns to add them all", tag, len(t.CIDRs), suggestMaxIPs)
		}
		pats = append(append([]string(nil), pats...), t.CIDRs...)
	}
	name := strings.TrimSpace(p.DisplayName)
	if name == "" {
		name = tagDisplayName(tag)
	}
	g, err := d.addUniqueCustomGroup(ctx, rules.CustomGroup{
		DisplayName: name,
		Description: "From geosite:" + tag,
		Patterns:    pats,
		Color:       groups.BrandColor(curatedKeyForTag(tag)),
		SourceTag:   tag,
	})
	if err != nil {
		return "", err
	}
	return g.Key, nil
}

// addUniqueCustomGroup inserts g under a key derived from its name,
// suffixing -2, -3, … when that key is taken (a curated-looking name like
// "Spotify" may already be a custom group).
func (d *handlerDeps) addUniqueCustomGroup(ctx context.Context, g rules.CustomGroup) (rules.CustomGroup, error) {
	base := rules.SlugifyGroupKey(g.DisplayName)
	if base == rules.CustomGroupPrefix {
		base = rules.SlugifyGroupKey("geosite-" + g.SourceTag)
	}
	for i := 1; i <= 50; i++ {
		g.Key = base
		if i > 1 {
			g.Key = fmt.Sprintf("%s-%d", base, i)
		}
		out, err := d.store.AddCustomGroup(ctx, g)
		if errors.Is(err, rules.ErrGroupDuplicate) {
			continue
		}
		return out, err
	}
	return rules.CustomGroup{}, rules.ErrGroupDuplicate
}

func (d *handlerDeps) suggestSplit(ctx context.Context, idx geo.SiteIndex, p ipc.SuggestSplitParams) (ipc.SuggestSplitResult, error) {
	res := ipc.SuggestSplitResult{Before: []ipc.GroupsUpdateParams{}}
	tag := normTag(p.Tag)
	if idx == nil {
		return res, errors.New("geosite data is not loaded")
	}
	t, ok := idx.Tag(tag)
	if !ok {
		return res, fmt.Errorf("unknown geosite tag %q", tag)
	}
	tagPats := tagPatterns(t)
	move := map[string]bool{}
	for _, mp := range p.Patterns {
		mp = normalizeGroupPattern(mp)
		// Only what really lies inside the tag may move: a split must
		// never carry an unrelated pattern into the new group.
		if mp != "" && insideTag(mp, tagPats) {
			move[mp] = true
		}
	}
	if len(move) == 0 {
		return res, errors.New("nothing to move: no pattern lies inside the tag")
	}

	type edit struct {
		before rules.CustomGroup
		keep   []string
	}
	var edits []edit
	var moved []string
	movedSeen := map[string]bool{}
	for _, key := range p.FromGroups {
		if !strings.HasPrefix(key, rules.CustomGroupPrefix) {
			return res, fmt.Errorf("%s is a built-in group; only custom groups can be split", key)
		}
		g, err := d.store.GetCustomGroup(ctx, key)
		if err != nil {
			return res, fmt.Errorf("%s: %w", key, err)
		}
		var keep []string
		took := false
		for _, gp := range g.Patterns {
			if move[normalizeGroupPattern(gp)] {
				took = true
				if !movedSeen[gp] {
					movedSeen[gp] = true
					moved = append(moved, gp)
				}
				continue
			}
			keep = append(keep, gp)
		}
		if took {
			edits = append(edits, edit{before: g, keep: keep})
		}
	}
	if len(moved) == 0 {
		return res, errors.New("nothing to move: the groups don't contain those patterns")
	}

	name := strings.TrimSpace(p.DisplayName)
	if name == "" {
		name = tagDisplayName(tag)
	}
	ng, err := d.addUniqueCustomGroup(ctx, rules.CustomGroup{
		DisplayName: name,
		Description: "From geosite:" + tag,
		Patterns:    moved,
		Color:       groups.BrandColor(curatedKeyForTag(tag)),
		SourceTag:   tag,
	})
	if err != nil {
		return res, err
	}
	for i, e := range edits {
		err := d.store.UpdateCustomGroup(ctx, rules.CustomGroup{
			Key:         e.before.Key,
			DisplayName: e.before.DisplayName,
			Description: e.before.Description,
			Patterns:    e.keep,
			Color:       e.before.Color,
		})
		if err != nil {
			// Put back what was already edited, drop the new group.
			for _, done := range edits[:i] {
				_ = d.store.UpdateCustomGroup(ctx, done.before)
			}
			_ = d.store.DeleteCustomGroup(ctx, ng.Key)
			return ipc.SuggestSplitResult{Before: []ipc.GroupsUpdateParams{}}, fmt.Errorf("update %s: %w", e.before.Key, err)
		}
		res.Before = append(res.Before, ipc.GroupsUpdateParams{
			Key:         e.before.Key,
			DisplayName: e.before.DisplayName,
			Description: e.before.Description,
			Patterns:    e.before.Patterns,
			Color:       e.before.Color,
		})
	}
	res.NewGroupKey = ng.Key
	return res, nil
}

// tagDrift compares a geosite group's definition with its tag now.
// tagNew: tag patterns neither the definition nor any stored rule covers
// (a newer geosite.dat added them). gone: definition patterns the tag no
// longer lists. IP ranges count only when the group took the tag's IPs.
func tagDrift(def []string, t geo.Tag, stored []rules.Rule) (tagNew, gone []string) {
	tagPats := tagPatterns(t)
	cidrs := map[string]bool{}
	for _, c := range t.CIDRs {
		cidrs[normalizeGroupPattern(c)] = true
	}
	defSet := map[string]bool{}
	tookIPs := false
	for _, p := range def {
		np := normalizeGroupPattern(p)
		defSet[np] = true
		if cidrs[np] {
			tookIPs = true
		}
	}
	for _, tp := range tagPats {
		if defSet[tp] || ruleBelongsToGroup(tp, def) || groupPatternApplied(tp, stored) {
			continue
		}
		tagNew = append(tagNew, tp)
	}
	if tookIPs {
		for _, c := range t.CIDRs {
			if !defSet[normalizeGroupPattern(c)] {
				tagNew = append(tagNew, c)
			}
		}
	}
	for _, p := range def {
		np := normalizeGroupPattern(p)
		if _, isIP := rules.ParseCIDR(np); isIP {
			if len(cidrs) > 0 && !cidrs[np] {
				gone = append(gone, p)
			}
			continue
		}
		if !insideTag(np, tagPats) {
			gone = append(gone, p)
		}
	}
	return tagNew, gone
}

// fillTagDrift sets SourceTag/TagNew/TagGone on a custom group's DTO.
// Without geo data loaded nothing is reported — never "everything gone".
func fillTagDrift(dto *ipc.GroupDTO, g rules.CustomGroup, idx geo.SiteIndex, stored []rules.Rule) {
	dto.SourceTag = g.SourceTag
	if g.SourceTag == "" || idx == nil {
		return
	}
	t, ok := idx.Tag(g.SourceTag)
	if !ok || len(t.Patterns) == 0 {
		return
	}
	dto.TagNew, dto.TagGone = tagDrift(g.Patterns, t, stored)
}

func (d *handlerDeps) groupsSyncTag(ctx context.Context, idx geo.SiteIndex, key string) error {
	g, err := d.store.GetCustomGroup(ctx, key)
	if err != nil {
		return err
	}
	if g.SourceTag == "" {
		return fmt.Errorf("%s was not created from a geosite tag", g.Key)
	}
	if idx == nil {
		return errors.New("geosite data is not loaded")
	}
	t, ok := idx.Tag(g.SourceTag)
	if !ok {
		return fmt.Errorf("geosite tag %s is not in the current data", g.SourceTag)
	}
	stored, err := d.store.List(ctx)
	if err != nil {
		return err
	}
	add, _ := tagDrift(g.Patterns, t, stored)
	if len(add) == 0 {
		return nil
	}
	g.Patterns = append(g.Patterns, add...)
	return d.store.UpdateCustomGroup(ctx, g)
}

// ---- names ----

// tagAliases maps geosite tags to curated group keys where the names
// differ.
var tagAliases = map[string]string{
	"TWITTER":  "x",
	"FACEBOOK": "meta",
	"CLAUDE":   "anthropic",
}

// curatedKeyForTag returns the curated group key matching a tag, or "".
func curatedKeyForTag(tag string) string {
	if k, ok := tagAliases[tag]; ok {
		return k
	}
	if g := groups.FindByKey(strings.ToLower(tag)); g != nil {
		return g.Key
	}
	return ""
}

// tagDisplayName derives a readable name from a geosite tag: a matching
// curated group's name ("SPOTIFY" → "Spotify"), else title-cased words
// with short tokens kept upper case ("APPLE-CN" → "Apple CN",
// "CATEGORY-GAMES" → "Games (category)", "CATEGORY-IR" → "IR (category)").
func tagDisplayName(tag string) string {
	tag = normTag(tag)
	if k := curatedKeyForTag(tag); k != "" {
		if g := groups.FindByKey(k); g != nil {
			return g.DisplayName
		}
	}
	category := false
	rest := tag
	if strings.HasPrefix(rest, "CATEGORY-") {
		category = true
		rest = strings.TrimPrefix(rest, "CATEGORY-")
	}
	words := strings.FieldsFunc(rest, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, w := range words {
		bare := strings.TrimPrefix(w, "!")
		if len(bare) <= 2 {
			words[i] = w // CN, IR, !CN, AI
			continue
		}
		words[i] = w[:1] + strings.ToLower(w[1:])
	}
	name := strings.Join(words, " ")
	if name == "" {
		name = tag
	}
	if category {
		name += " (category)"
	}
	return name
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
