package geo

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func writeFixture(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	site := EncodeGeoSite(map[string][]Domain{
		"SPOTIFY": {
			{Type: DomainDomain, Value: "spotify.com"},
			{Type: DomainDomain, Value: "scdn.co"},
			{Type: DomainFull, Value: "Open.Spotify.com."}, // case + trailing dot normalised
			{Type: DomainPlain, Value: "spotify"},
			{Type: DomainRegex, Value: `^spotify\d+\.net$`},
		},
		"CATEGORY-MUSIC": {
			{Type: DomainDomain, Value: "spotify.com"},
			{Type: DomainDomain, Value: "deezer.com"},
			{Type: DomainDomain, Value: "tidal.com"},
			{Type: DomainDomain, Value: "soundcloud.com"},
		},
		"SPOTIFY-OPEN": {{Type: DomainFull, Value: "open.spotify.com", Attrs: []string{"cn"}}},
		"CN":           {{Type: DomainDomain, Value: "spotify.com"}}, // Broad: never returned
		"TELEGRAM":     {{Type: DomainDomain, Value: "t.me"}},
	})
	ip := EncodeGeoIP(map[string][]string{
		"TELEGRAM": {"91.108.4.0/22", "2001:67c:4e8::/48"},
		"DE":       {"5.0.0.0/8", "2a00::/12"},
		"NL":       {"5.10.0.0/16"}, // nested inside DE's /8
		"PRIVATE":  {"10.0.0.0/8"},
	})
	return Open(Paths{
		GeoSite:  writeFixture(t, dir, "geosite.dat", site),
		GeoIP:    writeFixture(t, dir, "geoip.dat", ip),
		CityMMDB: filepath.Join(dir, "missing-city.mmdb"),
		ASNMMDB:  filepath.Join(dir, "missing-asn.mmdb"),
	})
}

func TestTagTranslation(t *testing.T) {
	db := fixtureDB(t)
	tag, ok := db.Tag("spotify")
	if !ok {
		t.Fatal("SPOTIFY not found")
	}
	want := []string{"*.scdn.co", "*.spotify.com", "open.spotify.com"}
	if !reflect.DeepEqual(tag.Patterns, want) {
		t.Errorf("patterns = %v, want %v", tag.Patterns, want)
	}
	if tag.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (keyword + regexp)", tag.Skipped)
	}
	if tag.CIDRs != nil {
		t.Errorf("SPOTIFY has no geoip tag, got CIDRs %v", tag.CIDRs)
	}
	tg, _ := db.Tag("TELEGRAM")
	if !reflect.DeepEqual(tg.CIDRs, []string{"91.108.4.0/22", "2001:67c:4e8::/48"}) {
		t.Errorf("telegram CIDRs = %v", tg.CIDRs)
	}
	if _, ok := db.Tag("NOPE"); ok {
		t.Error("unknown tag found")
	}
}

func TestTagsForMostSpecificFirst(t *testing.T) {
	db := fixtureDB(t)
	cases := map[string][]string{
		// Exact entries (depth 0) beat the domain entries at the parent;
		// at one depth the smaller tag comes first.
		"open.spotify.com":   {"SPOTIFY-OPEN", "SPOTIFY", "CATEGORY-MUSIC"},
		"OPEN.SPOTIFY.COM.":  {"SPOTIFY-OPEN", "SPOTIFY", "CATEGORY-MUSIC"},
		"api.spotify.com":    {"SPOTIFY", "CATEGORY-MUSIC"},
		"spotify.com":        {"SPOTIFY", "CATEGORY-MUSIC"},
		"x.open.spotify.com": {"SPOTIFY", "CATEGORY-MUSIC"}, // full: doesn't cover subs
		"deezer.com":         {"CATEGORY-MUSIC"},
		"t.me":               {"TELEGRAM"},
		"notspotify.com":     nil,
		"":                   nil,
	}
	for name, want := range cases {
		if got := db.TagsFor(name); !reflect.DeepEqual(got, want) {
			t.Errorf("TagsFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLocateCountryFallback(t *testing.T) {
	db := fixtureDB(t)
	loc, ok := db.Locate(netip.MustParseAddr("5.1.2.3"))
	if !ok || loc.Country != "DE" || loc.Precision != PrecisionCountry || loc.CountryName != "Germany" || loc.Lat == 0 {
		t.Errorf("5.1.2.3 → %+v %v", loc, ok)
	}
	if loc, _ := db.Locate(netip.MustParseAddr("5.10.9.9")); loc.Country != "NL" {
		t.Errorf("nested NL range → %q", loc.Country)
	}
	if loc, _ := db.Locate(netip.MustParseAddr("5.11.0.1")); loc.Country != "DE" {
		t.Errorf("after nested range → %q, want DE", loc.Country)
	}
	if loc, _ := db.Locate(netip.MustParseAddr("2a00:1450::1")); loc.Country != "DE" {
		t.Errorf("v6 → %q", loc.Country)
	}
	if loc, _ := db.Locate(netip.MustParseAddr("::ffff:5.1.2.3")); loc.Country != "DE" {
		t.Errorf("mapped v4 → %q", loc.Country)
	}
	// PRIVATE is not a country; 1.1.1.1 is in no tag.
	for _, ip := range []string{"10.1.1.1", "1.1.1.1"} {
		if _, ok := db.Locate(netip.MustParseAddr(ip)); ok {
			t.Errorf("%s located", ip)
		}
	}
}

func TestReloadKeepsOldOnError(t *testing.T) {
	db := fixtureDB(t)
	if len(db.TagsFor("t.me")) == 0 {
		t.Fatal("fixture not loaded")
	}
	if err := os.WriteFile(db.paths.GeoSite, []byte{0xff, 0xff, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Reload(); err == nil {
		t.Fatal("corrupt geosite reloaded without error")
	}
	if got := db.TagsFor("t.me"); !reflect.DeepEqual(got, []string{"TELEGRAM"}) {
		t.Errorf("after failed reload TagsFor = %v", got)
	}
}

func TestZeroAndMissing(t *testing.T) {
	var db DB
	if _, ok := db.Locate(netip.MustParseAddr("1.1.1.1")); ok {
		t.Error("zero DB located")
	}
	empty := Open(Paths{GeoSite: filepath.Join(t.TempDir(), "x.dat")})
	if empty.TagsFor("a.com") != nil {
		t.Error("missing file answered")
	}
	if err := empty.Reload(); err != nil {
		t.Errorf("missing files must not be an error: %v", err)
	}
}

// Against the installed geosite.dat: load time, index size and resident
// growth. Skipped where em-wall isn't installed.
func TestRealGeoSiteMemory(t *testing.T) {
	const site = "/usr/local/share/em-wall/geosite.dat"
	if _, err := os.Stat(site); err != nil {
		t.Skip("no installed geosite.dat")
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	db := Open(Paths{GeoSite: site, GeoIP: "/usr/local/share/em-wall/geoip.dat"})
	start := time.Now()
	tags := db.TagsFor("open.spotify.com")
	built := time.Since(start)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	st := db.Stats()
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("geosite: %d tags, index %.1f MB, heap +%.1f MB, first TagsFor %v → %v",
		st.GeoSiteTags, float64(st.IndexBytes)/1e6, float64(heap)/1e6, built, tags)
	if heap > 40<<20 {
		t.Errorf("index heap %d MB over the 40 MB target", heap>>20)
	}

	start = time.Now()
	loc, ok := db.Locate(netip.MustParseAddr("8.8.8.8"))
	t.Logf("geoip fallback: first Locate %v → %+v %v", time.Since(start), loc, ok)

	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("heap with country ranges: +%.1f MB", float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/1e6)
	runtime.KeepAlive(db)
}

func BenchmarkTagsFor(b *testing.B) {
	const site = "/usr/local/share/em-wall/geosite.dat"
	if _, err := os.Stat(site); err != nil {
		b.Skip("no installed geosite.dat")
	}
	db := Open(Paths{GeoSite: site})
	db.TagsFor("warmup.com")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.TagsFor("rr3---sn-4g5edn6r.googlevideo.com")
	}
}

// Readers never lock: lookups racing a reload see the old or the new
// snapshot, never a torn one. Run with -race.
func TestConcurrentReload(t *testing.T) {
	db := fixtureDB(t)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if got := db.TagsFor("t.me"); len(got) != 1 {
					t.Errorf("TagsFor during reload = %v", got)
					return
				}
				db.Locate(netip.MustParseAddr("5.1.2.3"))
				db.Tag("SPOTIFY")
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := db.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
}
