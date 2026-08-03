package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// Dynamic groups (core/groups/dynamic.go) carry a vendor feed URL instead of
// a fixed pattern list. This file owns the daemon half: fetch the feed, cache
// the parsed patterns in settings, and — when the user has actually applied
// the group — reconcile the stored rules so added/removed vendor prefixes
// show up as added/removed rules (and kernel routes) without user action.
//
// The cache is the source of truth for the group's patterns once populated;
// the Group's baked-in Patterns are only the pre-first-fetch seed.

const (
	// dynGroupSettingPrefix + <group key> is the settings row holding the
	// cached feed result. Settings live in the same SQLite DB as rules.
	dynGroupSettingPrefix = "dynamic_group:"

	dynGroupFetchTimeout = 30 * time.Second
	// Wait before the first fetch. At boot the daemon is up seconds before
	// the network settles and before the xray inbounds are listening, so an
	// immediate attempt loses both the direct fetch AND every via-proxy
	// fallback (connection refused on a port nothing has bound yet).
	dynGroupStartDelay = 90 * time.Second
	// How often the scheduler wakes to see whether any group is due. Due-ness
	// itself is per-group (DynamicSource.EffectiveInterval), so a short tick
	// costs one settings read — it exists so a failed fetch retries soon.
	dynGroupSchedTick = 5 * time.Minute
	// After a failed fetch, retry sooner than the full interval — a failure
	// at boot usually just means the network wasn't up yet.
	dynGroupRetryAfter = 10 * time.Minute
	// Cap the via-proxy fallback attempts when a direct fetch fails.
	dynGroupMaxFallback = 4
)

// dynamicGroupCache is the JSON persisted per dynamic group.
type dynamicGroupCache struct {
	Patterns  []string  `json:"patterns"`
	FetchedAt time.Time `json:"fetchedAt"`
	Source    string    `json:"source"`
	Error     string    `json:"error,omitempty"`
}

// due reports whether the group should be re-fetched now.
func (c dynamicGroupCache) due(now time.Time, interval time.Duration) bool {
	if c.FetchedAt.IsZero() || len(c.Patterns) == 0 {
		return true
	}
	if c.Error != "" {
		return now.Sub(c.FetchedAt) >= dynGroupRetryAfter
	}
	return now.Sub(c.FetchedAt) >= interval
}

func dynGroupSettingKey(groupKey string) string { return dynGroupSettingPrefix + groupKey }

// dynamicGroupCached reads the cached feed result for a group key. A missing
// or unparseable row yields the zero value (treated as "never fetched").
func (d *handlerDeps) dynamicGroupCached(ctx context.Context, groupKey string) dynamicGroupCache {
	raw, err := d.store.GetSetting(ctx, dynGroupSettingKey(groupKey), "")
	if err != nil || raw == "" {
		return dynamicGroupCache{}
	}
	var c dynamicGroupCache
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return dynamicGroupCache{}
	}
	return c
}

func (d *handlerDeps) saveDynamicGroupCache(ctx context.Context, groupKey string, c dynamicGroupCache) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := d.store.SetSetting(ctx, dynGroupSettingKey(groupKey), string(b)); err != nil {
		log.Printf("em-walld: dynamic group %q: cache write failed: %v", groupKey, err)
	}
}

// effectiveGroupPatterns is what the rest of the daemon should treat as the
// group's pattern list: the cached live feed once we have one, the baked-in
// seed until then. Static groups pass through untouched.
func (d *handlerDeps) effectiveGroupPatterns(ctx context.Context, g groups.Group) []string {
	if g.Dynamic == nil {
		return g.Patterns
	}
	if c := d.dynamicGroupCached(ctx, g.Key); len(c.Patterns) > 0 {
		return c.Patterns
	}
	return g.Patterns
}

// runDynamicGroupScheduler refreshes due dynamic groups on start and then on
// a ticker until ctx is cancelled.
func (d *handlerDeps) runDynamicGroupScheduler(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(dynGroupStartDelay):
	}
	d.refreshDueDynamicGroups(ctx)
	t := time.NewTicker(dynGroupSchedTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.refreshDueDynamicGroups(ctx)
		}
	}
}

func (d *handlerDeps) refreshDueDynamicGroups(ctx context.Context) {
	now := time.Now()
	for _, g := range groups.KnownGroups() {
		if g.Dynamic == nil {
			continue
		}
		if !d.dynamicGroupCached(ctx, g.Key).due(now, g.Dynamic.EffectiveInterval()) {
			continue
		}
		if _, _, err := d.refreshDynamicGroup(ctx, g); err != nil {
			log.Printf("em-walld: dynamic group %q: refresh failed: %v", g.Key, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// refreshAllDynamicGroups force-refreshes every dynamic group regardless of
// due time (the manual "refresh" IPC path).
func (d *handlerDeps) refreshAllDynamicGroups(ctx context.Context) []ipc.DynamicGroupDTO {
	out := []ipc.DynamicGroupDTO{}
	for _, g := range groups.KnownGroups() {
		if g.Dynamic == nil {
			continue
		}
		added, removed, err := d.refreshDynamicGroup(ctx, g)
		res := ipc.DynamicGroupDTO{Key: g.Key, Added: added, Removed: removed}
		if err != nil {
			res.Error = err.Error()
		}
		c := d.dynamicGroupCached(ctx, g.Key)
		res.Patterns = len(c.Patterns)
		if !c.FetchedAt.IsZero() {
			res.FetchedAt = c.FetchedAt.Format(time.RFC3339)
		}
		out = append(out, res)
	}
	return out
}

// refreshDynamicGroup fetches one group's feed, caches the parsed patterns,
// and reconciles applied rules against the change. Returns how many rules
// were added / removed.
//
// On any failure the previous cache is kept (only its Error/FetchedAt are
// stamped) — a bad fetch must never shrink a group the user relies on.
func (d *handlerDeps) refreshDynamicGroup(ctx context.Context, g groups.Group) (added, removed int, err error) {
	src := g.Dynamic
	if src == nil {
		return 0, 0, fmt.Errorf("group %q is not dynamic", g.Key)
	}
	prev := d.dynamicGroupCached(ctx, g.Key)

	feed, err := d.fetchFeed(ctx, src.URL)
	if err != nil {
		d.saveDynamicGroupCache(ctx, g.Key, dynamicGroupCache{
			Patterns: prev.Patterns, Source: src.URL,
			FetchedAt: time.Now(), Error: err.Error(),
		})
		return 0, 0, err
	}
	var exclude []byte
	if src.ExcludeURL != "" {
		// A missing exclusion feed is not fatal, but it WOULD widen the group
		// to include GCP customer ranges — so treat it as a failed refresh
		// and keep the previous list rather than over-routing.
		exclude, err = d.fetchFeed(ctx, src.ExcludeURL)
		if err != nil {
			d.saveDynamicGroupCache(ctx, g.Key, dynamicGroupCache{
				Patterns: prev.Patterns, Source: src.URL,
				FetchedAt: time.Now(), Error: "exclusion feed: " + err.Error(),
			})
			return 0, 0, fmt.Errorf("exclusion feed: %w", err)
		}
	}

	next, err := src.Parse(feed, exclude)
	if err != nil {
		d.saveDynamicGroupCache(ctx, g.Key, dynamicGroupCache{
			Patterns: prev.Patterns, Source: src.URL,
			FetchedAt: time.Now(), Error: err.Error(),
		})
		return 0, 0, err
	}

	// What the group covered before this fetch — seed on the very first run.
	before := prev.Patterns
	if len(before) == 0 {
		before = g.Patterns
	}

	d.saveDynamicGroupCache(ctx, g.Key, dynamicGroupCache{
		Patterns: next, Source: src.URL, FetchedAt: time.Now(),
	})

	added, removed = d.reconcileDynamicGroupRules(ctx, before, next)
	if added > 0 || removed > 0 {
		log.Printf("em-walld: dynamic group %q: %d prefixes (+%d rules, -%d rules)", g.Key, len(next), added, removed)
	} else {
		log.Printf("em-walld: dynamic group %q: %d prefixes, no rule changes", g.Key, len(next))
	}
	return added, removed, nil
}

// reconcileDynamicGroupRules brings the stored rules in line with a group's
// new pattern list.
//
// A group only has rules if the user applied it, so we first look for stored
// IP rules matching the OLD or NEW pattern set. None → the group isn't
// applied and we touch nothing (the refreshed cache alone is enough; a later
// apply will use it). Otherwise the first matching rule is the template:
// new prefixes are added with its action/interface/enabled state, and rules
// for prefixes the vendor dropped are deleted along with their routes.
func (d *handlerDeps) reconcileDynamicGroupRules(ctx context.Context, before, next []string) (added, removed int) {
	nextSet := make(map[string]struct{}, len(next))
	for _, p := range next {
		nextSet[p] = struct{}{}
	}
	beforeSet := make(map[string]struct{}, len(before))
	for _, p := range before {
		beforeSet[p] = struct{}{}
	}

	all, err := d.store.List(ctx)
	if err != nil {
		log.Printf("em-walld: dynamic group reconcile: list rules: %v", err)
		return 0, 0
	}

	var members []rules.Rule
	have := map[string]struct{}{}
	for _, r := range all {
		if !rules.IsIPRule(r.Pattern) {
			continue
		}
		p := normalizeGroupPattern(r.Pattern)
		_, inBefore := beforeSet[p]
		_, inNext := nextSet[p]
		if !inBefore && !inNext {
			continue
		}
		members = append(members, r)
		have[p] = struct{}{}
	}
	if len(members) == 0 {
		return 0, 0 // group not applied — nothing to reconcile
	}

	tpl := members[0]
	for _, p := range next {
		if _, ok := have[p]; ok {
			continue
		}
		r := rules.Rule{
			Pattern: p, Action: tpl.Action,
			Interface: tpl.Interface, Enabled: tpl.Enabled,
		}
		if _, err := d.store.Add(ctx, r); err != nil {
			if err.Error() != rules.ErrDuplicate.Error() {
				log.Printf("em-walld: dynamic group reconcile: add %s: %v", p, err)
			}
			continue
		}
		added++
	}
	for _, r := range members {
		if _, ok := nextSet[normalizeGroupPattern(r.Pattern)]; ok {
			continue
		}
		if d.router != nil {
			_ = d.router.RemoveByRule(ctx, r.ID)
		}
		d.proxyTable.RemoveByRule(r.ID)
		if err := d.store.Delete(ctx, r.ID); err != nil {
			continue
		}
		removed++
	}

	if added > 0 || removed > 0 {
		_ = d.engine.Reload(ctx)
		d.reconcileIPRoutes(ctx)
	}
	return added, removed
}

// fetchFeed GETs url directly, falling back through configured outbounds if
// the direct attempt fails — the feed host itself can be unreachable on the
// very networks this app exists to work around.
func (d *handlerDeps) fetchFeed(ctx context.Context, url string) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, dynGroupFetchTimeout)
	body, err := httpGetBody(dctx, directHTTPClient(dynGroupFetchTimeout), url)
	cancel()
	if err == nil {
		return body, nil
	}
	firstErr := err

	for i, p := range d.feedFallbackProxies(ctx) {
		if i >= dynGroupMaxFallback {
			break
		}
		cli, cerr := proxyHTTPClient(p, dynGroupFetchTimeout)
		if cerr != nil {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, dynGroupFetchTimeout)
		body, err := httpGetBody(vctx, cli, url)
		cancel()
		if err == nil {
			return body, nil
		}
	}
	return nil, fmt.Errorf("direct fetch failed (%v); no fallback outbound succeeded", firstErr)
}

// feedFallbackProxies orders candidates the same way subscription fetching
// does: user proxies first, then the hidden _xray_ SOCKS inbounds.
func (d *handlerDeps) feedFallbackProxies(ctx context.Context) []proxy.Proxy {
	if d.proxyStore == nil {
		return nil
	}
	all, err := d.proxyStore.List(ctx)
	if err != nil {
		return nil
	}
	var user, hidden []proxy.Proxy
	for _, p := range all {
		if xray.IsInternalProxyName(p.Name) {
			hidden = append(hidden, p)
		} else {
			user = append(user, p)
		}
	}
	return append(user, hidden...)
}

// dynGroupMaxFeedBytes caps a feed read. goog.json is ~30 KB; 4 MB is a
// generous ceiling that still stops a misrouted response from eating memory.
const dynGroupMaxFeedBytes = 4 << 20

func httpGetBody(ctx context.Context, cli *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, dynGroupMaxFeedBytes))
}
