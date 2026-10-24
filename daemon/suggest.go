package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/selfuse"
)

// Group suggestions: every DNS name no rule matched is offered here (from
// the query log sink, behind the DNS path) and looked up in the geosite
// index. Each name is credited to ONE tag, its most specific suggestible
// one (bestTag), and names em-wall resolves itself (core/selfuse) are not
// counted at all. A tag that keeps showing up and that no group covers
// becomes a suggestion in the UI. Everything here is in memory and
// advisory — it never writes a rule, and a daemon restart simply starts
// counting again.

const (
	// suggestQueue bounds names waiting for a lookup. Offer never blocks:
	// on a full queue the name is dropped (the next query for it counts).
	suggestQueue = 1024
	// suggestWindowDays is how long a sighting counts towards a tag.
	suggestWindowDays = 7
	// suggestSeenNames caps the hostnames remembered per tag.
	suggestSeenNames = 10
	// suggestMinHits / suggestMinSpan: a tag needs this many queries in the
	// window, spread over at least this long, before it is shown — one page
	// load fires a burst of lookups within seconds, and that isn't yet a
	// site the user uses.
	suggestMinHits = 5
	suggestMinSpan = 10 * time.Minute
	// suggestSettingsEvery is how often run re-reads suggest_enabled (it is
	// written through the generic settings.set, which has no hook here).
	suggestSettingsEvery = 30 * time.Second

	settingSuggestEnabled   = "suggest_enabled"
	settingSuggestDismissed = "suggest_dismissed"
	settingSuggestSnoozed   = "suggest_snoozed"
)

// settingsReader is the slice of *rules.Store the suggester needs.
type settingsReader interface {
	GetSetting(ctx context.Context, key, def string) (string, error)
}

// suggester counts geosite tags seen in un-ruled DNS traffic.
type suggester struct {
	idx      geo.SiteIndex
	settings settingsReader
	ch       chan string
	enabled  atomic.Bool
	now      func() time.Time

	// self is em-wall's own hostnames, never counted. selfLoad (set before
	// run starts; nil = the static table only) rebuilds it every
	// selfHostsEvery.
	self     atomic.Pointer[selfuse.Set]
	selfLoad func(context.Context) selfuse.Set

	mu   sync.Mutex
	tags map[string]*tagSeen
	// okTag caches suggestibleTag per tag; cleared on prune so a geo
	// reload is picked up.
	okTag map[string]bool
}

// dayCount is one day's hits; day is days since the Unix epoch (UTC).
type dayCount struct {
	day int64
	n   int
}

type tagSeen struct {
	days  [suggestWindowDays]dayCount // ring indexed by day % window
	first time.Time                   // first sighting since the tag was last empty
	last  time.Time
	names []string // most recent first, capped at suggestSeenNames
}

// tagStat is one tag's window, as returned by snapshot.
type tagStat struct {
	Tag       string
	Hits      int
	FirstSeen time.Time
	LastSeen  time.Time
	Names     []string
}

// newSuggester returns a suggester reading names against idx. It starts
// enabled; run keeps that in step with the suggest_enabled setting.
func newSuggester(idx geo.SiteIndex, settings settingsReader) *suggester {
	s := &suggester{
		idx:      idx,
		settings: settings,
		ch:       make(chan string, suggestQueue),
		now:      time.Now,
		tags:     make(map[string]*tagSeen),
		okTag:    make(map[string]bool),
	}
	self := selfuse.NewSet(selfuse.Static())
	s.self.Store(&self)
	s.enabled.Store(true)
	return s
}

// selfSet returns em-wall's own hostnames as last loaded.
func (s *suggester) selfSet() selfuse.Set {
	if s == nil {
		return nil
	}
	if p := s.self.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *suggester) refreshSelf(ctx context.Context) {
	if s.selfLoad == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	set := s.selfLoad(rctx)
	s.self.Store(&set)
}

// Offer queues a queried name for a lookup. Safe on the DNS path: it never
// blocks and never allocates beyond the channel send. Nil-safe.
func (s *suggester) Offer(name string) {
	if s == nil || !s.enabled.Load() || name == "" {
		return
	}
	select {
	case s.ch <- name:
	default:
	}
}

// run drains the queue until ctx ends.
func (s *suggester) run(ctx context.Context) {
	s.refreshEnabled(ctx)
	s.refreshSelf(ctx)
	t := time.NewTicker(suggestSettingsEvery)
	defer t.Stop()
	selfT := time.NewTicker(selfHostsEvery)
	defer selfT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-s.ch:
			s.observe(name)
		case <-t.C:
			s.refreshEnabled(ctx)
			s.prune()
		case <-selfT.C:
			s.refreshSelf(ctx)
		}
	}
}

func (s *suggester) refreshEnabled(ctx context.Context) {
	if s.settings == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := s.settings.GetSetting(rctx, settingSuggestEnabled, "true")
	if err != nil {
		return
	}
	on := v != "false"
	if s.enabled.Swap(on) && !on {
		// Turned off: forget what was seen, so turning it back on doesn't
		// resurface traffic from while it was off.
		s.mu.Lock()
		s.tags = make(map[string]*tagSeen)
		s.mu.Unlock()
	}
}

// observe credits one name to its best tag. The caller has already
// dropped names a rule matched (see storeLogSink.Log); em-wall's own
// names are dropped here.
func (s *suggester) observe(name string) {
	if !s.enabled.Load() || s.idx == nil {
		return
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	// CDN/cloud edge names say which network serves a site, not which
	// site it is (geo.EdgeDomains).
	if name == "" || s.selfSet().Has(name) || geo.IsEdge(name) {
		return
	}
	now := s.now()
	day := dayOf(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	tag := s.bestTag(name)
	if tag == "" {
		return
	}
	ts := s.tags[tag]
	if ts == nil {
		ts = &tagSeen{first: now}
		s.tags[tag] = ts
	}
	slot := &ts.days[day%suggestWindowDays]
	if slot.day != day {
		*slot = dayCount{day: day}
	}
	slot.n++
	ts.last = now
	ts.names = pushFront(ts.names, name, suggestSeenNames)
}

// bestTag is the one suggestible tag name is credited to: a geosite
// "full:" match first, then the longest matching domain, then a tag that
// is a curated group, then the smallest tag. Without geo.SiteMatcher the
// index's TagsFor order (already most specific first) decides. "" = no
// suggestible tag lists name. Called under s.mu.
func (s *suggester) bestTag(name string) string {
	var ms []geo.TagMatch
	if m, ok := s.idx.(geo.SiteMatcher); ok {
		ms = m.MatchesFor(name)
	} else {
		for i, t := range s.idx.TagsFor(name) {
			ms = append(ms, geo.TagMatch{Tag: t, Suffix: -i})
		}
	}
	var best geo.TagMatch
	bestCurated := false
	for _, m := range ms {
		if !s.suggestibleTag(m.Tag) {
			continue
		}
		curated := curatedKeyForTag(m.Tag) != ""
		if best.Tag != "" && !betterMatch(m, curated, best, bestCurated) {
			continue
		}
		best, bestCurated = m, curated
	}
	return best.Tag
}

func betterMatch(a geo.TagMatch, aCurated bool, b geo.TagMatch, bCurated bool) bool {
	switch {
	case a.Full != b.Full:
		return a.Full
	case a.Suffix != b.Suffix:
		return a.Suffix > b.Suffix
	case aCurated != bCurated:
		return aCurated
	case a.Size != b.Size:
		return a.Size < b.Size
	}
	return a.Tag < b.Tag
}

// suggestibleTag: geo.Suggestible (no categories, catch-alls,
// "!"/"@" variants), and no "-CN" regional variant of a tag that exists on
// its own (APPLE-CN → APPLE). Called under s.mu.
func (s *suggester) suggestibleTag(tag string) bool {
	if ok, done := s.okTag[tag]; done {
		return ok
	}
	ok := geo.Suggestible(tag)
	if base, cut := strings.CutSuffix(tag, "-CN"); ok && cut && hasSiteTag(s.idx, base) {
		ok = false
	}
	s.okTag[tag] = ok
	return ok
}

// siteTagLister is the cheap existence check *geo.DB has.
type siteTagLister interface {
	HasTag(site bool, tag string) bool
}

// hasSiteTag reports whether idx lists tag, cheaply when it can.
func hasSiteTag(idx geo.SiteIndex, tag string) bool {
	if h, ok := idx.(siteTagLister); ok {
		return h.HasTag(true, tag)
	}
	_, ok := idx.Tag(tag)
	return ok
}

// snapshot returns every tag with hits in the window, most hits first.
func (s *suggester) snapshot() []tagStat {
	if s == nil {
		return nil
	}
	today := dayOf(s.now())
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tagStat, 0, len(s.tags))
	for tag, ts := range s.tags {
		h := ts.hits(today)
		if h == 0 {
			continue
		}
		out = append(out, tagStat{
			Tag:       tag,
			Hits:      h,
			FirstSeen: ts.first,
			LastSeen:  ts.last,
			Names:     append([]string(nil), ts.names...),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

// forget drops one tag's counts (after it became a group).
func (s *suggester) forget(tag string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.tags, tag)
	s.mu.Unlock()
}

// prune drops tags with nothing left in the window.
func (s *suggester) prune() {
	today := dayOf(s.now())
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, ts := range s.tags {
		if ts.hits(today) == 0 {
			delete(s.tags, tag)
		}
	}
	clear(s.okTag)
}

func (ts *tagSeen) hits(today int64) int {
	n := 0
	for _, d := range ts.days {
		if d.day > today-suggestWindowDays && d.day <= today {
			n += d.n
		}
	}
	return n
}

func dayOf(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// pushFront moves v to the front of list (adding it if absent), capped.
func pushFront(list []string, v string, max int) []string {
	for i, x := range list {
		if x == v {
			copy(list[1:i+1], list[:i])
			list[0] = v
			return list
		}
	}
	if len(list) < max {
		list = append(list, "")
	}
	copy(list[1:], list[:len(list)-1])
	list[0] = v
	return list
}

// activeSuggester is the daemon's suggester, installed by main. The query
// log sink holds its own reference; the IPC handlers read this one. Nil
// (not installed) means suggest.list reports nothing.
var activeSuggester atomic.Pointer[suggester]
