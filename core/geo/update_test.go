package geo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/geo/meta"
)

// fakeRelease serves a GitHub-shaped release: /releases/latest/download/F
// redirects to /releases/download/<tag>/F and on to /cdn/F, which serves files[F].
type fakeRelease struct {
	mu    sync.Mutex
	tag   string
	files map[string][]byte // name → body; "<name>.sha256sum" served when present
	hits  map[string]int
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[r.URL.Path]++
	if rest, ok := strings.CutPrefix(r.URL.Path, "/releases/latest/download/"); ok {
		http.Redirect(w, r, "/releases/download/"+f.tag+"/"+rest, http.StatusFound)
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/releases/download/"); ok {
		// GitHub sends the tagged URL on to a CDN path without the tag.
		http.Redirect(w, r, "/cdn/"+rest[strings.IndexByte(rest, '/')+1:], http.StatusFound)
		return
	}
	name := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	b, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(b)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

var (
	oldSite = EncodeGeoSite(map[string][]Domain{
		"SPOTIFY":  {{Type: DomainDomain, Value: "spotify.com"}},
		"TELEGRAM": {{Type: DomainDomain, Value: "t.me"}},
	})
	newSite = EncodeGeoSite(map[string][]Domain{
		"SPOTIFY":  {{Type: DomainDomain, Value: "spotify.com"}, {Type: DomainDomain, Value: "scdn.co"}},
		"TELEGRAM": {{Type: DomainDomain, Value: "t.me"}},
	})
	siteWithoutSpotify = EncodeGeoSite(map[string][]Domain{
		"TELEGRAM": {{Type: DomainDomain, Value: "t.me"}},
	})
)

type updaterEnv struct {
	u         *Updater
	dir       string
	rel       *fakeRelease
	srv       *httptest.Server
	installed [][]string
	required  Required
	now       time.Time
}

func newUpdaterEnv(t *testing.T) *updaterEnv {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "geosite.dat"), oldSite, 0o644); err != nil {
		t.Fatal(err)
	}
	env := &updaterEnv{dir: dir, rel: &fakeRelease{tag: "202610230000", files: map[string][]byte{}},
		now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	env.srv = httptest.NewServer(env.rel)
	t.Cleanup(env.srv.Close)
	db := Open(Paths{GeoSite: filepath.Join(dir, "geosite.dat"), GeoIP: filepath.Join(dir, "geoip.dat"),
		CityMMDB: filepath.Join(dir, meta.FileNames[meta.City]), ASNMMDB: filepath.Join(dir, meta.FileNames[meta.ASN])})
	env.u = &Updater{
		Dir: dir,
		DB:  db,
		Clients: func(context.Context) []Client {
			return []Client{{Name: "direct", HTTP: env.srv.Client()}}
		},
		Source: func(context.Context) (Source, error) {
			base := env.srv.URL + "/releases/latest/download/"
			return Source{Key: "test", GeoSiteURL: base + "geosite.dat", GeoIPURL: base + "geoip.dat"}, nil
		},
		Required:    func(context.Context) (Required, error) { return env.required, nil },
		OnInstalled: func(_ context.Context, keys []string) { env.installed = append(env.installed, keys) },
		Now:         func() time.Time { return env.now },
	}
	if err := env.u.SyncMeta(); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *updaterEnv) serve(name string, body []byte, checksum string) {
	e.rel.mu.Lock()
	defer e.rel.mu.Unlock()
	e.rel.files[name] = body
	if checksum != "" {
		e.rel.files[name+".sha256sum"] = []byte(checksum + "  " + name + "\n")
	} else {
		delete(e.rel.files, name+".sha256sum")
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdateInstallsAndRotates(t *testing.T) {
	env := newUpdaterEnv(t)
	if got := env.u.DB.TagsFor("x.scdn.co"); got != nil {
		t.Fatalf("old file already lists scdn.co: %v", got)
	}
	m, _ := meta.Read(env.dir)
	if m[meta.GeoSite].Source != SourceBundled || !env.u.Due(m, meta.GeoSite) {
		t.Fatalf("a bundled geosite must be recorded and due: %+v", m[meta.GeoSite])
	}

	env.serve("geosite.dat", newSite, sum(newSite))
	if errs := env.u.Update(context.Background(), []string{meta.GeoSite}); errs != nil {
		t.Fatalf("update: %v", errs)
	}
	path := filepath.Join(env.dir, "geosite.dat")
	if !bytes.Equal(readFile(t, path), newSite) || !bytes.Equal(readFile(t, path+".bak"), oldSite) {
		t.Fatal("file not installed / old one not rotated to .bak")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".tmp left behind")
	}
	m, _ = meta.Read(env.dir)
	e := m[meta.GeoSite]
	if e.Source != "test" || e.Version != "202610230000" || e.SHA256 != sum(newSite) || e.Prev == nil || e.Prev.Source != SourceBundled {
		t.Errorf("meta = %+v", e)
	}
	if env.u.Due(m, meta.GeoSite) {
		t.Error("due right after an update")
	}
	if got := env.u.DB.TagsFor("x.scdn.co"); !reflect.DeepEqual(got, []string{"SPOTIFY"}) {
		t.Errorf("DB not reloaded: TagsFor = %v", got)
	}
	if !reflect.DeepEqual(env.installed, [][]string{{meta.GeoSite}}) {
		t.Errorf("OnInstalled calls = %v", env.installed)
	}

	// The same bytes again: a successful check, nothing rotated.
	env.now = env.now.Add(8 * 24 * time.Hour)
	if errs := env.u.Update(context.Background(), []string{meta.GeoSite}); errs != nil {
		t.Fatal(errs)
	}
	if !bytes.Equal(readFile(t, path+".bak"), oldSite) || len(env.installed) != 1 {
		t.Error("identical download rotated the backup or fired OnInstalled")
	}
	m, _ = meta.Read(env.dir)
	if !m[meta.GeoSite].CheckedAt.Equal(env.now) {
		t.Errorf("CheckedAt not advanced: %v", m[meta.GeoSite].CheckedAt)
	}
}

// assertUntouched checks the live file and its absence of .bak after a
// rejected update, and that the error was recorded.
func assertRejected(t *testing.T, env *updaterEnv, errs map[string]error, want string) {
	t.Helper()
	err := errs[meta.GeoSite]
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
	path := filepath.Join(env.dir, "geosite.dat")
	if !bytes.Equal(readFile(t, path), oldSite) {
		t.Error("live file replaced by a rejected download")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("backup rotated by a rejected download")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error(".tmp left behind")
	}
	m, _ := meta.Read(env.dir)
	if m[meta.GeoSite].LastError == "" || m[meta.GeoSite].LastAttempt.IsZero() {
		t.Errorf("failure not recorded: %+v", m[meta.GeoSite])
	}
	if m[meta.GeoSite].Source != SourceBundled {
		t.Errorf("meta source changed: %+v", m[meta.GeoSite])
	}
	if env.u.Due(m, meta.GeoSite) {
		t.Error("due again right after a failure (no backoff)")
	}
	if len(env.installed) != 0 {
		t.Errorf("OnInstalled fired: %v", env.installed)
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	env := newUpdaterEnv(t)
	var viaHits int
	env.u.Clients = func(context.Context) []Client {
		via := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			viaHits++
			return http.DefaultTransport.RoundTrip(r)
		})}
		return []Client{{Name: "direct", HTTP: env.srv.Client()}, {Name: "via test", HTTP: via}}
	}
	env.serve("geosite.dat", newSite, sum(oldSite))
	assertRejected(t, env, env.u.Update(context.Background(), []string{meta.GeoSite}), "checksum mismatch")
	if viaHits != 0 {
		t.Errorf("a checksum mismatch was retried through the next client (%d requests)", viaHits)
	}
}

func TestUpdateRejectsUnparseable(t *testing.T) {
	env := newUpdaterEnv(t)
	junk := []byte("<html>rate limited</html>")
	env.serve("geosite.dat", junk, sum(junk))
	assertRejected(t, env, env.u.Update(context.Background(), []string{meta.GeoSite}), "does not parse")
}

func TestUpdateRejectsMissingReferencedTag(t *testing.T) {
	env := newUpdaterEnv(t)
	// NOPE is referenced but the current file lacks it too: not held against the new one.
	env.required = Required{Site: []string{"SPOTIFY", "NOPE"}}
	env.serve("geosite.dat", siteWithoutSpotify, sum(siteWithoutSpotify))
	assertRejected(t, env, env.u.Update(context.Background(), []string{meta.GeoSite}), "SPOTIFY")

	env2 := newUpdaterEnv(t)
	env2.required = Required{Site: []string{"NOPE"}}
	env2.serve("geosite.dat", siteWithoutSpotify, sum(siteWithoutSpotify))
	if errs := env2.u.Update(context.Background(), []string{meta.GeoSite}); errs != nil {
		t.Errorf("a tag missing from both files blocked the update: %v", errs)
	}
}

func TestUpdateFallsBackToVia(t *testing.T) {
	env := newUpdaterEnv(t)
	env.serve("geosite.dat", newSite, "") // no checksum published: parse is the check
	dead := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	env.u.Clients = func(context.Context) []Client {
		return []Client{{Name: "direct", HTTP: dead}, {Name: "via xrayset:nj", HTTP: env.srv.Client()}}
	}
	if errs := env.u.Update(context.Background(), []string{meta.GeoSite}); errs != nil {
		t.Fatalf("via fallback: %v", errs)
	}
	if !bytes.Equal(readFile(t, filepath.Join(env.dir, "geosite.dat")), newSite) {
		t.Error("not installed through the via client")
	}
}

func TestRollbackSwapsAndRollsForward(t *testing.T) {
	env := newUpdaterEnv(t)
	if err := env.u.Rollback(context.Background()); err == nil {
		t.Error("rollback with no backups succeeded")
	}
	env.serve("geosite.dat", newSite, sum(newSite))
	if errs := env.u.Update(context.Background(), []string{meta.GeoSite}); errs != nil {
		t.Fatal(errs)
	}
	path := filepath.Join(env.dir, "geosite.dat")

	if err := env.u.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !bytes.Equal(readFile(t, path), oldSite) || !bytes.Equal(readFile(t, path+".bak"), newSite) {
		t.Fatal("rollback did not swap file and backup")
	}
	m, _ := meta.Read(env.dir)
	if e := m[meta.GeoSite]; e.Source != SourceBundled || e.SHA256 != sum(oldSite) || e.Prev == nil || e.Prev.Version != "202610230000" {
		t.Errorf("meta after rollback = %+v", e)
	}
	if env.u.Due(m, meta.GeoSite) {
		t.Error("a rolled-back file is due at once: the auto-updater would undo the rollback")
	}
	if got := env.u.DB.TagsFor("x.scdn.co"); got != nil {
		t.Errorf("DB not reloaded after rollback: %v", got)
	}

	// A backup that would break xray is refused.
	env.required = Required{Site: []string{"SPOTIFY"}}
	if err := os.WriteFile(path+".bak", siteWithoutSpotify, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.u.Rollback(context.Background()); err == nil || !strings.Contains(err.Error(), "SPOTIFY") {
		t.Errorf("rollback to a backup lacking a referenced tag: %v", err)
	}
	if !bytes.Equal(readFile(t, path), oldSite) {
		t.Error("refused rollback still replaced the file")
	}
}

func TestUpdateMMDBFallsBackToLastMonth(t *testing.T) {
	env := newUpdaterEnv(t)
	city := filepath.Join(t.TempDir(), "city.mmdb")
	fixtureCity(t, city)
	gz := gzipFile(t, city)

	mux := http.NewServeMux()
	var asked []string
	mux.HandleFunc("/free/", func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path == "/free/dbip-city-lite-2026-09.mmdb.gz" {
			w.Write(gz)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	old := DBIPBase
	DBIPBase = srv.URL + "/free/"
	defer func() { DBIPBase = old }()
	env.u.Clients = func(context.Context) []Client { return []Client{{Name: "direct", HTTP: srv.Client()}} }

	m, _ := meta.Read(env.dir)
	if _, ok := env.u.NextDue(m, meta.City); ok {
		t.Error("a never-fetched mmdb must only come on demand")
	}
	env.u.EnsureMMDB(context.Background())
	if !reflect.DeepEqual(asked[:2], []string{"/free/dbip-city-lite-2026-10.mmdb.gz", "/free/dbip-city-lite-2026-09.mmdb.gz"}) {
		t.Errorf("asked %v", asked)
	}
	m, _ = meta.Read(env.dir)
	if e := m[meta.City]; e.Version != "2026-09" || e.Source != SourceDBIP {
		t.Errorf("city meta = %+v", e)
	}
	if loc, _ := env.u.DB.Locate(mustAddr("1.1.1.1")); loc.City != "Sydney" {
		t.Errorf("city data not loaded: %+v", loc)
	}
	// The asn file isn't served: recorded as a failure, not retried at once.
	if m[meta.ASN].LastError == "" {
		t.Errorf("asn failure not recorded: %+v", m[meta.ASN])
	}
	n := len(asked)
	env.u.EnsureMMDB(context.Background())
	if len(asked) != n {
		t.Error("EnsureMMDB retried a failure inside the backoff")
	}

	// Last month installed, this month still 404: a clean check.
	env.now = env.now.Add(25 * time.Hour)
	if !env.u.Due(m, meta.City) {
		t.Fatal("last month's file not due for a recheck after a day")
	}
	if errs := env.u.Update(context.Background(), []string{meta.City}); errs != nil {
		t.Errorf("recheck: %v", errs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestScanRequired(t *testing.T) {
	got := ScanRequired(
		`[{"domain":["geosite:spotify","geosite:google@cn","domain:x.com"],"outboundTag":"a"},{"ip":["geoip:!cn","geoip:telegram"]}]`,
		`{"protocol":"freedom","settings":{"note":"geosite:category-ads-all"}}`,
		`{"domain":["ext:custom.dat:foo"]}`,
	)
	want := Required{Site: []string{"CATEGORY-ADS-ALL", "GOOGLE", "SPOTIFY"}, IP: []string{"CN", "TELEGRAM"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScanRequired = %+v, want %+v", got, want)
	}
}

func TestResolveSource(t *testing.T) {
	s, err := ResolveSource("", "")
	if err != nil || s.Key != SourceLoyalsoldier || !strings.Contains(s.GeoSiteURL, "Loyalsoldier") {
		t.Errorf("default = %+v %v", s, err)
	}
	if s, _ := ResolveSource("v2fly", ""); !strings.HasSuffix(s.GeoSiteURL, "/dlc.dat") {
		t.Errorf("v2fly = %+v", s)
	}
	if s, _ := ResolveSource("custom", "https://example.com/geo/"); s.GeoIPURL != "https://example.com/geo/geoip.dat" {
		t.Errorf("custom = %+v", s)
	}
	if _, err := ResolveSource("custom", "ftp://x"); err == nil {
		t.Error("custom without http(s) accepted")
	}
	if _, err := ResolveSource("nope", ""); err == nil {
		t.Error("unknown source accepted")
	}
	if got := releaseTag("https://github.com/x/y/releases/download/202610232213/geosite.dat"); got != "202610232213" {
		t.Errorf("releaseTag = %q", got)
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
