// Package geo reads the offline geo data em-wall ships or downloads:
// xray's geosite.dat / geoip.dat (v2fly protobuf) and DB-IP Lite City/ASN
// (.mmdb). The readers are pure Go and offline; fetching lives in update.go.
//
// Two consumers, two narrow interfaces:
//   - Locator: IP → where it is (the world map).
//   - SiteIndex: hostname → which geosite tags list it (group suggestions).
//
// *DB satisfies both. Its data is swapped whole on Reload (atomic.Pointer),
// so readers never lock and a failed reload keeps the previous data.
package geo

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Precision of a Location.
const (
	PrecisionCity    = "city"    // DB-IP City answered
	PrecisionCountry = "country" // only geoip.dat answered (centroid coordinates)
)

// Location is where an IP is. Lat/Lon are always set when ok: a city's
// coordinates, or the country centroid when Precision is "country".
type Location struct {
	Country     string  `json:"country"`     // ISO 3166-1 alpha-2, upper case
	CountryName string  `json:"countryName"` // English
	City        string  `json:"city"`        // "" at country precision
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Precision   string  `json:"precision"`
	ASN         uint32  `json:"asn"` // 0 = unknown
	Org         string  `json:"org"` // ASN organisation, "" = unknown
	// Anycast is true when the ASN is a known anycast network (Cloudflare,
	// Google, Akamai, …): the pinned city is only one of many edges.
	Anycast bool `json:"anycast"`
}

// Locator places an IP on the map.
type Locator interface {
	Locate(ip netip.Addr) (Location, bool)
}

// Tag is one geosite tag translated to em-wall patterns.
type Tag struct {
	Name string // upper case, as stored in geosite.dat (e.g. "SPOTIFY")
	// Patterns are em-wall rule patterns: geosite "domain:x" → "*.x",
	// "full:x" → "x". Sorted, deduplicated.
	Patterns []string
	// CIDRs are the same-named geoip tag's ranges (e.g. geoip:telegram),
	// nil when geoip.dat has no such tag.
	CIDRs []string
	// Skipped counts keyword/regexp entries em-wall can't express.
	Skipped int
}

// SiteIndex answers which geosite tags list a hostname.
type SiteIndex interface {
	// TagsFor returns the tags whose domain/full entries match name,
	// most specific match first. Catch-all tags (see Broad) are excluded.
	TagsFor(name string) []string
	// Tag returns one tag's translated content.
	Tag(name string) (Tag, bool)
}

// TagMatch is one tag listing a hostname, and how: Full is a geosite
// "full:" (exact) entry, Suffix the length of the matched entry's domain
// (longer = more specific), Size the tag's indexed entry count.
type TagMatch struct {
	Tag    string
	Full   bool
	Suffix int
	Size   int
}

// SiteMatcher is a SiteIndex that can also say how each tag matched. The
// suggester uses it to credit a name to its most specific tag; a SiteIndex
// without it is read through TagsFor's order.
type SiteMatcher interface {
	MatchesFor(name string) []TagMatch
}

// EdgeDomains are domains a CDN or cloud platform serves OTHER sites
// from. A lookup of one says which network carries a site, not which site
// it is, so it never counts toward a suggestion; and a group built from a
// tag never takes them, or routing "Cloudflare" would route every site
// behind Cloudflare's CDN. The companies themselves stay suggestible:
// dash.cloudflare.com, the AWS console and the Azure portal are not here.
var EdgeDomains = []string{
	// Akamai
	"akamaiedge.net", "akamaized.net", "akamaihd.net", "akamai.net",
	"akadns.net", "edgekey.net", "edgesuite.net", "akamaistream.net", "srip.net",
	// Cloudflare
	"cloudflare.net", "cdnjs.cloudflare.com", "cloudflareinsights.com",
	"workers.dev", "pages.dev", "r2.dev", "trycloudflare.com",
	// Fastly
	"fastly.net", "fastlylb.net", "fastly-edge.com",
	// AWS
	"cloudfront.net", "amazonaws.com", "awsglobalaccelerator.com",
	// Azure
	"azureedge.net", "azurefd.net", "azurewebsites.net", "cloudapp.net",
	"cloudapp.azure.com", "trafficmanager.net", "windows.net",
	// Google Cloud / Firebase hosting
	"appspot.com", "run.app", "cloudfunctions.net", "web.app", "firebaseapp.com",
	"storage.googleapis.com",
	// Other CDNs and app hosts
	"edgecastcdn.net", "llnwd.net", "cdn77.org", "b-cdn.net", "jsdelivr.net",
	"aliyuncs.com", "alikunlun.com", "myqcloud.com", "tencent-cloud.net",
	"vercel.app", "netlify.app", "herokuapp.com",
}

var edgeSet = func() map[string]bool {
	m := make(map[string]bool, len(EdgeDomains))
	for _, d := range EdgeDomains {
		m[d] = true
	}
	return m
}()

// IsEdge reports whether name is, or is under, an EdgeDomains entry.
func IsEdge(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for {
		if edgeSet[name] {
			return true
		}
		dot := strings.IndexByte(name, '.')
		if dot < 0 {
			return false
		}
		name = name[dot+1:]
	}
}

// IsEdgePattern is IsEdge for an em-wall pattern ("*.x" or "x").
func IsEdgePattern(p string) bool { return IsEdge(strings.TrimPrefix(p, "*.")) }

// Suggestible reports whether tag can be offered as a group on its own:
// not Broad, not a category ("CATEGORY-*", "TLD-*",
// "GEOLOCATION-*"), and not an attribute/negation variant ("!" or "@").
// Regional "-CN" variants are the caller's call — they are fine when no
// base tag exists.
func Suggestible(tag string) bool {
	tag = strings.ToUpper(tag)
	if tag == "" || Broad[tag] || strings.ContainsAny(tag, "!@") {
		return false
	}
	for _, p := range []string{"CATEGORY-", "TLD-", "GEOLOCATION-"} {
		if strings.HasPrefix(tag, p) {
			return false
		}
	}
	return true
}

// Broad lists tags too wide to be "a site" — never suggested, never used
// for collision labels.
var Broad = map[string]bool{
	"CN": true, "GEOLOCATION-CN": true, "GEOLOCATION-!CN": true,
	"CATEGORY-ADS": true, "CATEGORY-ADS-ALL": true, "PRIVATE": true,
	"TLD-CN": true, "TLD-!CN": true,
	// Catch-all lists in the Loyalsoldier / Chocolate4U builds: a name in
	// them says nothing about which site it belongs to.
	"CHINA-LIST": true, "GFW": true, "GREATFIRE": true, "PROXY": true,
	"DIRECT": true, "REJECT": true, "CATEGORY-COMPANIES": true,
	"EASYLIST": true, "CATEGORY-ADS-IR": true, "TLD-RU": true,
	"TLD-OPENNIC": true, "IR": true, "TLD-IR": true,
}

// Paths of the files a DB loads. Any may be missing; the DB then answers
// with what it has (country-only without the mmdb, nothing without .dat).
type Paths struct {
	GeoSite  string
	GeoIP    string
	CityMMDB string
	ASNMMDB  string
}

// DB is the loaded geo data. Zero value answers nothing; call Reload.
type DB struct {
	paths Paths
	cur   atomic.Pointer[data]
	// loadMu serialises Reload and the lazy first load.
	loadMu sync.Mutex
	loaded bool // a load was attempted (Open's lazy first use); under loadMu
}

// data is one immutable snapshot of everything loaded. The lazily built
// parts (site index, country ranges) hang off it behind sync.Once, so a
// reload discards them with the snapshot.
type data struct {
	site *datFile // nil when geosite.dat is missing
	ip   *datFile // nil when geoip.dat is missing
	city *maxminddb.Reader
	asn  *maxminddb.Reader

	idxOnce sync.Once
	idx     atomic.Pointer[siteIndex] // set once built; nil on build failure

	rangesOnce sync.Once
	ranges     *countryRanges
}

// Open returns a DB for paths without loading anything yet: the index is
// built lazily on first use (it costs tens of MB) or by Reload.
func Open(p Paths) *DB { return &DB{paths: p} }

// Paths returns the files this DB reads.
func (db *DB) Paths() Paths { return db.paths }

// Reload rebuilds the snapshot from disk. On error the old one stays.
// Missing files are not an error — the DB answers with what it has.
func (db *DB) Reload() error {
	db.loadMu.Lock()
	defer db.loadMu.Unlock()
	return db.reloadLocked()
}

func (db *DB) reloadLocked() error {
	db.loaded = true
	d, err := load(db.paths)
	if err != nil {
		return err
	}
	old := db.cur.Swap(d)
	if old != nil {
		// mmdb readers are mmapped; unmap only once nobody can be using
		// them. Readers grab the snapshot pointer per call, so give
		// in-flight lookups a moment before closing.
		go closeLater(old)
	}
	return nil
}

func load(p Paths) (*data, error) {
	d := &data{}
	var err error
	if d.site, err = openDat(p.GeoSite); err != nil {
		return nil, err
	}
	if d.ip, err = openDat(p.GeoIP); err != nil {
		return nil, err
	}
	if d.city, err = openMMDB(p.CityMMDB); err != nil {
		return nil, err
	}
	if d.asn, err = openMMDB(p.ASNMMDB); err != nil {
		if d.city != nil {
			d.city.Close()
		}
		return nil, err
	}
	return d, nil
}

// snap returns the current snapshot, loading it on first use.
func (db *DB) snap() *data {
	if d := db.cur.Load(); d != nil {
		return d
	}
	db.loadMu.Lock()
	defer db.loadMu.Unlock()
	if d := db.cur.Load(); d != nil || db.loaded {
		return d // a failed first load isn't retried per call; Reload retries
	}
	_ = db.reloadLocked()
	return db.cur.Load()
}

// Locate places ip: city precision from the DB-IP City mmdb, else the
// geoip.dat country at its centroid; ASN/Org from the ASN mmdb either way.
func (db *DB) Locate(ip netip.Addr) (Location, bool) {
	d := db.snap()
	if d == nil || !ip.IsValid() {
		return Location{}, false
	}
	ip = ip.Unmap()
	loc, ok := d.locateCity(ip)
	if !ok && d.ip != nil {
		d.rangesOnce.Do(func() {
			// A failed build leaves ranges nil: country lookups then
			// answer nothing for this snapshot, which is the honest result.
			d.ranges, _ = buildCountryRanges(d.ip)
		})
		if d.ranges != nil {
			if cc := d.ranges.country(ip); cc != "" {
				c := centroids()[cc]
				loc = Location{Country: cc, CountryName: c.name, Lat: c.lat, Lon: c.lon, Precision: PrecisionCountry}
				ok = true
			}
		}
	}
	if !ok {
		return Location{}, false
	}
	if asn, org, found := d.lookupASN(ip); found {
		loc.ASN, loc.Org, loc.Anycast = asn, org, IsAnycast(asn)
	}
	return loc, true
}

// TagsFor returns the non-Broad tags listing name, most specific first.
// The first call per snapshot builds the reverse index.
func (db *DB) TagsFor(name string) []string {
	idx := db.siteIdx()
	if idx == nil {
		return nil
	}
	return idx.tagsFor(name)
}

// siteIdx returns the snapshot's reverse index, building it on first use;
// nil without geosite data.
func (db *DB) siteIdx() *siteIndex {
	d := db.snap()
	if d == nil || d.site == nil {
		return nil
	}
	d.idxOnce.Do(func() {
		// A failed build leaves the index nil: no suggestions for this
		// snapshot rather than wrong ones.
		if idx, err := buildSiteIndex(d.site); err == nil {
			d.idx.Store(idx)
		}
	})
	return d.idx.Load()
}

// MatchesFor returns every non-Broad tag listing name and how it matched
// (see SiteMatcher), deepest match first.
func (db *DB) MatchesFor(name string) []TagMatch {
	idx := db.siteIdx()
	if idx == nil {
		return nil
	}
	return idx.matchesFor(name)
}

// Tag returns one tag's translated patterns, plus the same-named geoip
// tag's CIDRs. Decoded from the file on each call: callers (suggestion
// listing, group sync) are infrequent and want few tags.
func (db *DB) Tag(name string) (Tag, bool) {
	d := db.snap()
	if d == nil || d.site == nil {
		return Tag{}, false
	}
	name = strings.ToUpper(strings.TrimSpace(name))
	s, ok := d.site.tags[name]
	if !ok {
		return Tag{}, false
	}
	msg, err := d.site.read(s)
	if err != nil {
		return Tag{}, false
	}
	t := Tag{Name: name}
	seen := map[string]bool{}
	if err := decodeSite(msg, func(dm Domain) {
		p, ok := sitePattern(dm)
		if !ok {
			t.Skipped++
			return
		}
		if !seen[p] {
			seen[p] = true
			t.Patterns = append(t.Patterns, p)
		}
	}); err != nil {
		return Tag{}, false
	}
	sort.Strings(t.Patterns)
	t.CIDRs = d.cidrs(name)
	return t, true
}

// cidrs returns geoip.dat's ranges for name, nil if it has no such tag.
func (d *data) cidrs(name string) []string {
	if d.ip == nil {
		return nil
	}
	s, ok := d.ip.tags[name]
	if !ok {
		return nil
	}
	msg, err := d.ip.read(s)
	if err != nil {
		return nil
	}
	out := []string{}
	_ = decodeIP(msg, func(p netip.Prefix) { out = append(out, p.Masked().String()) })
	return out
}

// HasTag reports whether the loaded geosite (site=true) or geoip file
// lists tag. Used to tell which referenced tags a file already provides.
func (db *DB) HasTag(site bool, tag string) bool {
	d := db.snap()
	if d == nil {
		return false
	}
	f := d.ip
	if site {
		f = d.site
	}
	if f == nil {
		return false
	}
	_, ok := f.tags[strings.ToUpper(tag)]
	return ok
}

// Stats is what Status reports per file: entry count (tags for .dat,
// search-tree nodes for .mmdb) and, for geosite, the reverse index size
// once built (0 before).
type Stats struct {
	GeoSiteTags, GeoIPTags int
	CityNodes, ASNNodes    int
	CityLoaded, ASNLoaded  bool
	IndexBytes             int
}

// Stats describes the current snapshot without building anything.
func (db *DB) Stats() Stats {
	var st Stats
	d := db.snap()
	if d == nil {
		return st
	}
	if d.site != nil {
		st.GeoSiteTags = len(d.site.tags)
	}
	if d.ip != nil {
		st.GeoIPTags = len(d.ip.tags)
	}
	if d.city != nil {
		st.CityLoaded, st.CityNodes = true, int(d.city.Metadata.NodeCount)
	}
	if d.asn != nil {
		st.ASNLoaded, st.ASNNodes = true, int(d.asn.Metadata.NodeCount)
	}
	if idx := d.idx.Load(); idx != nil {
		st.IndexBytes = idx.memBytes()
	}
	return st
}

var (
	_ Locator   = (*DB)(nil)
	_ SiteIndex = (*DB)(nil)
)
