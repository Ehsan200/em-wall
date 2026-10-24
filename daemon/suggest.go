package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/geo"
)

// Group suggestions: every DNS name no rule matched is offered here (from
// the query log sink, behind the DNS path) and looked up in the geosite
// index. A tag that keeps showing up and that no group covers becomes a
// suggestion card in the UI. Everything here is in memory and advisory —
// it never writes a rule, and a daemon restart simply starts counting
// again.

const (
	// suggestQueue bounds names waiting for a lookup. Offer never blocks:
	// on a full queue the name is dropped (the next query for it counts).
	suggestQueue = 1024
	// suggestWindowDays is how long a sighting counts towards a tag.
	suggestWindowDays = 7
	// suggestSeenNames caps the hostnames remembered per tag.
	suggestSeenNames = 10
	// suggestMinHits: a tag needs this many queries in the window before it
	// is shown — one stray lookup isn't a site the user visits.
	suggestMinHits = 3
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

	mu   sync.Mutex
	tags map[string]*tagSeen
}

// dayCount is one day's hits; day is days since the Unix epoch (UTC).
type dayCount struct {
	day int64
	n   int
}

type tagSeen struct {
	days  [suggestWindowDays]dayCount // ring indexed by day % window
	last  time.Time
	names []string // most recent first, capped at suggestSeenNames
}

// tagStat is one tag's window, as returned by snapshot.
type tagStat struct {
	Tag      string
	Hits     int
	LastSeen time.Time
	Names    []string
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
	}
	s.enabled.Store(true)
	return s
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
	t := time.NewTicker(suggestSettingsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-s.ch:
			s.observe(name)
		case <-t.C:
			s.refreshEnabled(ctx)
			s.prune()
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

// observe looks one name up and counts its tags. The caller has already
// dropped names a rule matched (see storeLogSink.Log).
func (s *suggester) observe(name string) {
	if !s.enabled.Load() || s.idx == nil {
		return
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" {
		return
	}
	tags := s.idx.TagsFor(name)
	if len(tags) == 0 {
		return
	}
	now := s.now()
	day := dayOf(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tag := range tags {
		if geo.Broad[tag] {
			continue
		}
		ts := s.tags[tag]
		if ts == nil {
			ts = &tagSeen{}
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
			Tag:      tag,
			Hits:     h,
			LastSeen: ts.last,
			Names:    append([]string(nil), ts.names...),
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
