package geo

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/geo/meta"
)

// The updater keeps the four geo files fresh without ever leaving a
// broken one in place. Per file: download to <file>.tmp, verify (published
// checksum, then a full parse with the reader), refuse a .dat that drops a
// tag the running xray config references, rotate <file> → <file>.bak,
// rename .tmp → <file>, record geo.meta, reload the DB. Every failure
// leaves the live file untouched.
//
// It knows nothing about the daemon: how to reach the network (direct,
// then through an em-wall outbound), which tags xray needs and what to
// do after a file changes are all supplied by the caller.

// Geosite/geoip source keys (setting geo_source).
const (
	SourceLoyalsoldier = "loyalsoldier"
	SourceChocolate4U  = "chocolate4u"
	SourceV2Fly        = "v2fly"
	SourceCustom       = "custom"
	SourceDBIP         = "dbip"
	SourceBundled      = "bundled" // shipped with the app; no meta entry yet
)

// Source is where the .dat files come from.
type Source struct {
	Key        string
	GeoSiteURL string
	GeoIPURL   string
}

// ResolveSource maps a geo_source setting to URLs. custom takes a base
// URL that serves geosite.dat and geoip.dat side by side.
func ResolveSource(key, customBase string) (Source, error) {
	gh := func(repo, site, ip string) Source {
		base := "https://github.com/" + repo + "/releases/latest/download/"
		return Source{GeoSiteURL: base + site, GeoIPURL: base + ip}
	}
	var s Source
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "", SourceLoyalsoldier:
		s, key = gh("Loyalsoldier/v2ray-rules-dat", "geosite.dat", "geoip.dat"), SourceLoyalsoldier
	case SourceChocolate4U:
		s = gh("Chocolate4U/Iran-v2ray-rules", "geosite.dat", "geoip.dat")
	case SourceV2Fly:
		s = Source{
			GeoSiteURL: "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat",
			GeoIPURL:   "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat",
		}
	case SourceCustom:
		base := strings.TrimRight(strings.TrimSpace(customBase), "/")
		if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
			return Source{}, fmt.Errorf("geo: custom source needs an http(s) base URL, got %q", customBase)
		}
		s = Source{GeoSiteURL: base + "/geosite.dat", GeoIPURL: base + "/geoip.dat"}
	default:
		return Source{}, fmt.Errorf("geo: unknown source %q", key)
	}
	s.Key = strings.ToLower(key)
	return s, nil
}

// DBIPBase serves DB-IP's free monthly databases. A var so tests can
// point it at a local server.
var DBIPBase = "https://download.db-ip.com/free/"

// dbipURL is the .mmdb.gz for kind ("city"/"asn") and month "YYYY-MM".
func dbipURL(kind, month string) string {
	return DBIPBase + "dbip-" + kind + "-lite-" + month + ".mmdb.gz"
}

// Client is one way to reach the network, tried in order.
type Client struct {
	Name string // "direct", "via xrayset:nj" — for logs and errors
	HTTP *http.Client
}

// Required are the geosite/geoip tags the running xray config references
// (upper case). A new file must still list each one it lists now.
type Required struct {
	Site, IP []string
}

// refRe finds geosite:/geoip: references in xray JSON. Attribute filters
// ("geosite:google@cn") and negation ("geoip:!cn") are stripped; ext:
// files are someone else's.
var refRe = regexp.MustCompile(`"(geosite|geoip):(!?)([A-Za-z0-9_\-.!]+)(@[A-Za-z0-9_\-!]+)?"`)

// ScanRequired collects the tags referenced by xray JSON documents
// (routing rules, outbound configs).
func ScanRequired(docs ...string) Required {
	site, ip := map[string]bool{}, map[string]bool{}
	for _, d := range docs {
		for _, m := range refRe.FindAllStringSubmatch(d, -1) {
			tag := strings.ToUpper(m[3])
			if m[1] == "geosite" {
				site[tag] = true
			} else {
				ip[tag] = true
			}
		}
	}
	return Required{Site: sortedKeys(site), IP: sortedKeys(ip)}
}

// Intervals.
const (
	DatInterval    = 7 * 24 * time.Hour
	mmdbRecheck    = 24 * time.Hour // after a check that found no newer month
	failureBackoff = 6 * time.Hour
	maxDatBytes    = 128 << 20
	maxMMDBBytes   = 1 << 30 // decompressed; DB-IP city lite is ~130 MB
	checksumBytes  = 4 << 10
)

// Updater runs updates for one directory. Fields are set before first use.
type Updater struct {
	Dir string
	DB  *DB // reloaded after each change; may be nil in tests

	// Clients returns the ways to fetch, in order (direct first).
	Clients func(ctx context.Context) []Client
	// Source returns the configured .dat source.
	Source func(ctx context.Context) (Source, error)
	// Required returns the tags the running xray config needs.
	Required func(ctx context.Context) (Required, error)
	// OnInstalled runs after files changed on disk (update or rollback)
	// and the DB was reloaded; keys are the changed files.
	OnInstalled func(ctx context.Context, keys []string)
	Now         func() time.Time
	Logf        func(format string, args ...any)

	mu   sync.Mutex // one update or rollback at a time
	busy atomic.Bool
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

// Busy reports whether an update or rollback is running.
func (u *Updater) Busy() bool { return u.busy.Load() }

func (u *Updater) path(key string) string { return filepath.Join(u.Dir, meta.FileNames[key]) }

// ErrBusy is returned when another update holds the lock.
var ErrBusy = errors.New("geo: an update is already running")

// SyncMeta reconciles geo.meta with what is on disk: a file without an
// entry, or whose bytes no longer match the entry's checksum (the
// installer laid down a newer bundled copy), is recorded as bundled.
// Run once at startup, before the first Due check.
func (u *Updater) SyncMeta() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	m, err := meta.Read(u.Dir)
	if err != nil {
		m = meta.Meta{} // unreadable: rebuild rather than wedge every update
	}
	changed := false
	for _, key := range meta.Files {
		fi, err := os.Stat(u.path(key))
		if err != nil {
			continue
		}
		e, ok := m[key]
		if ok && e.SHA256 == "" {
			continue // an entry without a checksum: trust it
		}
		sum, err := fileSHA256(u.path(key))
		if err != nil || (ok && sum == e.SHA256) {
			continue
		}
		m[key] = meta.Entry{Source: SourceBundled, SHA256: sum, UpdatedAt: fi.ModTime().UTC()}
		changed = true
	}
	if !changed {
		return nil
	}
	return meta.Write(u.Dir, m)
}

// Due reports whether key should be fetched by the auto-updater now.
// The .dat files are weekly (a bundled copy is due at once: it is as old
// as the release). The mmdb files are monthly, and only once present —
// first fetch is on demand (EnsureMMDB).
func (u *Updater) Due(m meta.Meta, key string) bool {
	next, ok := u.NextDue(m, key)
	return ok && !u.now().Before(next)
}

// NextDue is when key next becomes due; ok is false when it never will
// on its own (an mmdb that was never fetched).
func (u *Updater) NextDue(m meta.Meta, key string) (time.Time, bool) {
	e, has := m[key]
	_, statErr := os.Stat(u.path(key))
	present := statErr == nil
	var next time.Time
	switch key {
	case meta.GeoSite, meta.GeoIP:
		if !present || !has || (e.Source == SourceBundled && e.CheckedAt.IsZero()) {
			next = time.Time{} // as old as the release that shipped it
		} else {
			next = latest(e.UpdatedAt, e.CheckedAt).Add(DatInterval)
		}
	case meta.City, meta.ASN:
		if !present {
			return time.Time{}, false
		}
		if has && e.Version >= u.now().UTC().Format("2006-01") {
			// Current month installed: due at the start of next month.
			t := u.now().UTC()
			next = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		} else {
			next = e.CheckedAt.Add(mmdbRecheck)
		}
	default:
		return time.Time{}, false
	}
	if !e.LastAttempt.IsZero() {
		next = latest(next, e.LastAttempt.Add(failureBackoff))
	}
	return next, true
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// RunDue updates every due file. Called by the hourly tick.
func (u *Updater) RunDue(ctx context.Context) map[string]error {
	m, err := meta.Read(u.Dir)
	if err != nil {
		m = meta.Meta{}
	}
	var keys []string
	for _, k := range meta.Files {
		if u.Due(m, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return u.Update(ctx, keys)
}

// Update fetches keys now, due or not. The result maps each key that
// failed to its error; nil means every one succeeded (changed or not).
func (u *Updater) Update(ctx context.Context, keys []string) map[string]error {
	if !u.mu.TryLock() {
		return map[string]error{"": ErrBusy}
	}
	u.busy.Store(true)
	defer u.unlock()
	return u.updateLocked(ctx, keys)
}

// Start runs Update in the background and returns at once — ErrBusy when
// one is already running, so Busy is true from the moment Start returns
// nil. done, if set, gets the result. ctx bounds the whole run.
func (u *Updater) Start(ctx context.Context, keys []string, done func(map[string]error)) error {
	if !u.mu.TryLock() {
		return ErrBusy
	}
	u.busy.Store(true)
	go func() {
		defer u.unlock()
		errs := u.updateLocked(ctx, keys)
		if done != nil {
			done(errs)
		}
	}()
	return nil
}

func (u *Updater) unlock() {
	u.busy.Store(false)
	u.mu.Unlock()
}

func (u *Updater) updateLocked(ctx context.Context, keys []string) map[string]error {
	// datErr, when set, fails both .dat files without touching the mmdb ones.
	var src Source
	var datErr error
	if u.Source != nil {
		src, datErr = u.Source(ctx)
	} else {
		src, datErr = ResolveSource("", "")
	}
	var req Required
	if u.Required != nil {
		var err error
		if req, err = u.Required(ctx); err != nil {
			// Without the reference list a new .dat can't be checked; skip
			// the .dat files rather than risk breaking xray.
			datErr = fmt.Errorf("geo: read xray references: %w", err)
		}
	}

	errs := map[string]error{}
	var changed []string
	for _, key := range keys {
		var err error
		var did bool
		switch key {
		case meta.GeoSite, meta.GeoIP:
			if datErr != nil {
				err = datErr
				break
			}
			did, err = u.updateDat(ctx, key, src, req)
		case meta.City, meta.ASN:
			did, err = u.updateMMDB(ctx, key)
		default:
			err = fmt.Errorf("geo: unknown file %q", key)
		}
		u.record(key, err)
		if err != nil {
			errs[key] = err
			u.logf("geo: update %s: %v", key, err)
			continue
		}
		if did {
			changed = append(changed, key)
		}
	}
	u.installed(ctx, changed)
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// installed reloads the DB and notifies the caller after a change.
func (u *Updater) installed(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	if u.DB != nil {
		if err := u.DB.Reload(); err != nil {
			u.logf("geo: reload after update: %v", err)
		}
	}
	if u.OnInstalled != nil {
		u.OnInstalled(ctx, keys)
	}
}

// record notes a failed attempt (or clears the last error) in geo.meta.
func (u *Updater) record(key string, err error) {
	m, rerr := meta.Read(u.Dir)
	if rerr != nil {
		m = meta.Meta{}
	}
	e := m[key]
	if err != nil {
		e.LastAttempt, e.LastError = u.now().UTC(), err.Error()
	} else {
		e.LastAttempt, e.LastError = time.Time{}, ""
	}
	m[key] = e
	if werr := meta.Write(u.Dir, m); werr != nil {
		u.logf("geo: write meta: %v", werr)
	}
}

// updateDat fetches one .dat. did is false when the download matched the
// installed file.
func (u *Updater) updateDat(ctx context.Context, key string, src Source, req Required) (bool, error) {
	url := src.GeoSiteURL
	need := req.Site
	if key == meta.GeoIP {
		url, need = src.GeoIPURL, req.IP
	}
	tmp := u.path(key) + ".tmp"
	defer os.Remove(tmp)

	var version string
	err := u.eachClient(ctx, func(c Client) error {
		chain, lastMod, err := download(ctx, c.HTTP, url, tmp, maxDatBytes, false)
		if err != nil {
			return err
		}
		want, err := fetchChecksum(ctx, c.HTTP, url+".sha256sum")
		if err != nil {
			return err
		}
		if want != "" {
			got, err := fileSHA256(tmp)
			if err != nil {
				return err
			}
			if got != want {
				// Not retried through another client: a mismatch is a bad
				// release or a tampering path, and either way not this file.
				return permanent(fmt.Errorf("checksum mismatch: got %s, published %s", got, want))
			}
		}
		version = lastMod
		for _, u := range chain {
			if tag := releaseTag(u); tag != "" {
				version = tag
				break
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	b, err := os.ReadFile(tmp)
	if err != nil {
		return false, err
	}
	df, err := scanDat(b)
	if err != nil {
		return false, fmt.Errorf("downloaded file does not parse: %w", err)
	}
	if len(df.tags) == 0 {
		return false, errors.New("downloaded file has no tags")
	}
	if missing := u.missingTags(key == meta.GeoSite, need, df); len(missing) > 0 {
		return false, fmt.Errorf("new file lacks %s referenced by the xray config: %s",
			plural(len(missing), "tag"), strings.Join(missing, ", "))
	}
	return u.install(key, tmp, meta.Entry{Source: src.Key, Version: version})
}

// missingTags lists the needed tags df lacks. Only tags the installed
// file has count: one already missing can't be broken further, and
// refusing every update over it would freeze the data for nothing.
func (u *Updater) missingTags(site bool, need []string, df *datFile) []string {
	var out []string
	for _, t := range need {
		if _, ok := df.tags[t]; ok {
			continue
		}
		if u.DB != nil && !u.DB.HasTag(site, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// updateMMDB fetches this month's DB-IP file, or last month's when this
// month's isn't published yet (DB-IP releases on the 1st, not always at
// midnight UTC).
func (u *Updater) updateMMDB(ctx context.Context, key string) (bool, error) {
	kind := key
	tmp := u.path(key) + ".tmp"
	defer os.Remove(tmp)

	t := u.now().UTC()
	thisMonth := t.Format("2006-01")
	lastMonth := time.Date(t.Year(), t.Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	m, _ := meta.Read(u.Dir)
	have := m[key].Version
	if have >= thisMonth {
		return false, u.touch(key)
	}
	months := []string{thisMonth}
	if have < lastMonth {
		months = append(months, lastMonth)
	}

	var version string
	err := u.eachClient(ctx, func(c Client) error {
		var lastErr error
		for _, mon := range months {
			_, _, err := download(ctx, c.HTTP, dbipURL(kind, mon), tmp, maxMMDBBytes, true)
			if err == nil {
				version = mon
				return nil
			}
			if !errors.Is(err, errNotFound) {
				return err
			}
			lastErr = err
		}
		return lastErr
	})
	if errors.Is(err, errNotFound) && have != "" && len(months) == 1 {
		// This month isn't out yet and last month's is what we have.
		return false, u.touch(key)
	}
	if err != nil {
		return false, err
	}
	if _, err := CheckMMDB(tmp, kind); err != nil {
		return false, fmt.Errorf("downloaded file does not verify: %w", err)
	}
	return u.install(key, tmp, meta.Entry{Source: SourceDBIP, Version: version})
}

// touch records a successful check that found nothing newer.
func (u *Updater) touch(key string) error {
	m, err := meta.Read(u.Dir)
	if err != nil {
		return err
	}
	e := m[key]
	e.CheckedAt = u.now().UTC()
	m[key] = e
	return meta.Write(u.Dir, m)
}

// install moves a verified tmp into place: current → .bak, tmp → file.
// Same bytes as installed → just a successful check.
func (u *Updater) install(key, tmp string, e meta.Entry) (bool, error) {
	sum, err := fileSHA256(tmp)
	if err != nil {
		return false, err
	}
	m, err := meta.Read(u.Dir)
	if err != nil {
		m = meta.Meta{}
	}
	cur, hasCur := m[key]
	if hasCur && cur.SHA256 == sum {
		return false, u.touch(key)
	}
	dst := u.path(key)
	if err := os.Chmod(tmp, 0o644); err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, dst+".bak"); err != nil {
			return false, fmt.Errorf("rotate backup: %w", err)
		}
		prev := cur
		prev.Prev = nil
		if !hasCur {
			prev = meta.Entry{Source: SourceBundled}
		}
		e.Prev = &prev
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Put the old file back so the live path is never empty.
		_ = os.Rename(dst+".bak", dst)
		return false, err
	}
	now := u.now().UTC()
	e.SHA256, e.UpdatedAt, e.CheckedAt = sum, now, now
	m[key] = e
	if err := meta.Write(u.Dir, m); err != nil {
		return true, err
	}
	u.logf("geo: installed %s %s from %s", key, e.Version, e.Source)
	return true, nil
}

// Rollback swaps every file that has a .bak with it (so a second rollback
// rolls forward again). A .bak that fails verification — corrupt, or a
// .dat lacking a tag xray now references — is left alone and reported.
func (u *Updater) Rollback(ctx context.Context) error {
	if !u.mu.TryLock() {
		return ErrBusy
	}
	u.busy.Store(true)
	defer u.unlock()

	var req Required
	if u.Required != nil {
		var err error
		if req, err = u.Required(ctx); err != nil {
			return fmt.Errorf("geo: read xray references: %w", err)
		}
	}
	m, err := meta.Read(u.Dir)
	if err != nil {
		m = meta.Meta{}
	}
	var changed, problems []string
	for _, key := range meta.Files {
		dst := u.path(key)
		bak := dst + ".bak"
		if _, err := os.Stat(bak); err != nil {
			continue
		}
		if err := u.verifyFile(key, bak, req); err != nil {
			problems = append(problems, fmt.Sprintf("%s: backup not usable: %v", key, err))
			continue
		}
		swap := dst + ".swap"
		_ = os.Remove(swap)
		if err := os.Rename(dst, swap); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		if err := os.Rename(bak, dst); err != nil {
			_ = os.Rename(swap, dst)
			problems = append(problems, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		_ = os.Rename(swap, bak)

		cur := m[key]
		prev := meta.Entry{Source: "unknown"}
		if cur.Prev != nil {
			prev = *cur.Prev
		}
		cur.Prev = nil
		prev.Prev = &cur
		prev.LastAttempt, prev.LastError = time.Time{}, ""
		if prev.SHA256 == "" {
			prev.SHA256, _ = fileSHA256(dst)
		}
		// Rolled back by hand: keep it for a full interval before the
		// auto-updater fetches over it again.
		prev.CheckedAt = u.now().UTC()
		m[key] = prev
		changed = append(changed, key)
	}
	if len(changed) > 0 {
		if err := meta.Write(u.Dir, m); err != nil {
			problems = append(problems, "write meta: "+err.Error())
		}
		u.logf("geo: rolled back %s", strings.Join(changed, ", "))
	}
	u.installed(ctx, changed)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	if len(changed) == 0 {
		return errors.New("geo: no backups to roll back to")
	}
	return nil
}

// verifyFile checks that path is a usable file of kind key.
func (u *Updater) verifyFile(key, path string, req Required) error {
	switch key {
	case meta.City, meta.ASN:
		_, err := CheckMMDB(path, key)
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	df, err := scanDat(b)
	if err != nil {
		return err
	}
	need := req.Site
	if key == meta.GeoIP {
		need = req.IP
	}
	if missing := u.missingTags(key == meta.GeoSite, need, df); len(missing) > 0 {
		return fmt.Errorf("lacks %s referenced by the xray config: %s", plural(len(missing), "tag"), strings.Join(missing, ", "))
	}
	return nil
}

// EnsureMMDB fetches the DB-IP files that are missing. It returns at once
// when both are present or an update is already running; the fetch runs
// on the caller's goroutine, so callers that must not block start it with
// go. A recent failure (failureBackoff) is not retried.
func (u *Updater) EnsureMMDB(ctx context.Context) {
	m, _ := meta.Read(u.Dir)
	var keys []string
	for _, k := range []string{meta.City, meta.ASN} {
		if _, err := os.Stat(u.path(k)); err == nil {
			continue
		}
		if e, ok := m[k]; ok && !e.LastAttempt.IsZero() && u.now().Sub(e.LastAttempt) < failureBackoff {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 || u.Busy() {
		return
	}
	u.Update(ctx, keys)
}

// FileStatus is one file's state for geo.status.
type FileStatus struct {
	Key       string
	Entry     meta.Entry
	Present   bool
	Size      int64
	HasBackup bool
	NextDue   time.Time // zero when auto-update would never fetch it
}

// Status describes every file.
func (u *Updater) Status() []FileStatus {
	m, err := meta.Read(u.Dir)
	if err != nil {
		m = meta.Meta{}
	}
	out := make([]FileStatus, 0, len(meta.Files))
	for _, k := range meta.Files {
		fs := FileStatus{Key: k, Entry: m[k]}
		if fi, err := os.Stat(u.path(k)); err == nil {
			fs.Present, fs.Size = true, fi.Size()
		}
		if _, err := os.Stat(u.path(k) + ".bak"); err == nil {
			fs.HasBackup = true
		}
		if next, ok := u.NextDue(m, k); ok {
			if next.Before(u.now()) {
				next = u.now()
			}
			fs.NextDue = next
		}
		out = append(out, fs)
	}
	return out
}

// eachClient runs fn with each client until one succeeds. A permanent
// error (bad checksum) stops the walk.
func (u *Updater) eachClient(ctx context.Context, fn func(Client) error) error {
	var clients []Client
	if u.Clients != nil {
		clients = u.Clients(ctx)
	} else {
		clients = []Client{{Name: "direct", HTTP: http.DefaultClient}}
	}
	if len(clients) == 0 {
		return errors.New("no way to reach the network")
	}
	var errs []string
	var last error
	for _, c := range clients {
		err := fn(c)
		if err == nil {
			return nil
		}
		last = err
		var p permanentErr
		if errors.As(err, &p) {
			return p.error
		}
		errs = append(errs, c.Name+": "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 1 {
		return last
	}
	return wrapAll{last, strings.Join(errs, "; ")}
}

// wrapAll keeps errors.Is working on the last error while reporting all.
type wrapAll struct {
	last error
	msg  string
}

func (w wrapAll) Error() string { return w.msg }
func (w wrapAll) Unwrap() error { return w.last }

type permanentErr struct{ error }

func permanent(err error) error { return permanentErr{err} }

var errNotFound = errors.New("not found (404)")

// download GETs url into dst (truncating), gunzipping when gz. It returns
// every URL of the redirect chain, last first (GitHub's releases/latest
// goes to releases/download/<tag>/ and then to a CDN URL without the
// tag), and the Last-Modified date.
func download(ctx context.Context, c *http.Client, url, dst string, limit int64, gz bool) (chain []string, lastMod string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "em-wall")
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %s", resp.Status)
	}
	var body io.Reader = resp.Body
	if gz {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("gunzip: %w", err)
		}
		defer zr.Close()
		body = zr
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", err
	}
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, "", err
	}
	if n > limit {
		return nil, "", fmt.Errorf("file larger than %d MB", limit>>20)
	}
	if resp.ContentLength > 0 && !gz && n != resp.ContentLength {
		return nil, "", fmt.Errorf("truncated: %d of %d bytes", n, resp.ContentLength)
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		lastMod = t.UTC().Format("2006-01-02")
	}
	for r := resp.Request; r != nil; {
		chain = append(chain, r.URL.String())
		if r.Response == nil {
			break
		}
		r = r.Response.Request
	}
	return chain, lastMod, nil
}

// fetchChecksum returns the hex sha256 published at url, "" when the
// source publishes none (404). Format: "<hex>  <filename>".
func fetchChecksum(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "em-wall")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum: HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, checksumBytes))
	if err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", permanent(fmt.Errorf("checksum file is malformed"))
	}
	if _, err := hex.DecodeString(f[0]); err != nil {
		return "", permanent(fmt.Errorf("checksum file is malformed"))
	}
	return strings.ToLower(f[0]), nil
}

// releaseTag pulls <tag> out of a GitHub ".../releases/download/<tag>/file"
// URL (where releases/latest redirects to).
func releaseTag(u string) string {
	const marker = "/releases/download/"
	i := strings.Index(u, marker)
	if i < 0 {
		return ""
	}
	rest := u[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j > 0 {
		return rest[:j]
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}
