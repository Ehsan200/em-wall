package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/version"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Self-update downloads survive failure. The .dmg streams into a
// persistent per-version cache instead of a temp file, so:
//
//   - a dropped connection resumes with an HTTP Range request from the
//     last byte on disk, retried with backoff (updateMaxAttempts);
//   - a stall (no bytes for updateStallTimeout) is treated as a drop —
//     without it a half-dead connection hangs the download forever;
//   - a failed run, a cancel or an app restart keeps the partial file, and
//     the next attempt continues from it;
//   - a complete, verified .dmg is reused as-is (no second download when
//     the mount or swap step failed for another reason).
//
// Layout in updateCacheDir(): <key>.dmg.part while downloading, renamed to
// <key>.dmg once complete and verified; <key>.dmg.json holds the URL and
// validators (ETag / Last-Modified) that make a resume safe. Files of any
// other release are pruned when a download starts.
const (
	updateMaxAttempts    = 8
	updateBackoffMax     = 30 * time.Second
	updateHeaderTimeout  = 20 * time.Second
	updateProgressPeriod = 250 * time.Millisecond
)

// Variables so tests can shorten them.
var (
	updateBackoffBase  = time.Second
	updateStallTimeout = 30 * time.Second
)

// updateTarget is one release asset to download.
type updateTarget struct {
	Version string
	URL     string
	Size    int64  // from the release API; 0 if unknown
	Digest  string // "sha256:<hex>" from the release API; "" if unknown
}

// updateMeta is persisted beside the partial file. A resume is only
// attempted when it describes the same URL.
type updateMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Size         int64  `json:"size,omitempty"`
}

// updateRetryEvent is emitted as "update:retry" before each backoff wait.
type updateRetryEvent struct {
	Attempt    int     `json:"attempt"` // the attempt that just failed
	Max        int     `json:"max"`
	WaitSec    float64 `json:"waitSec"`
	Error      string  `json:"error"`
	Downloaded int64   `json:"downloaded"` // bytes kept on disk
	Size       int64   `json:"size"`
}

// permanentError marks a failure retrying can't fix (404, bad digest
// twice, disk errors).
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

var errUpdateStalled = errors.New("download stalled")

var updateHTTP = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = updateHeaderTimeout
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Transport: tr}
}()

func (a *App) rememberUpdateTarget(t updateTarget) {
	a.updMu.Lock()
	a.updTarget = &t
	a.updMu.Unlock()
}

// updateTargetFor returns the remembered release for url, or a bare
// target (cache key derived from the URL) when CheckForUpdate hasn't
// described it.
func (a *App) updateTargetFor(url string) updateTarget {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	if a.updTarget != nil && a.updTarget.URL == url {
		return *a.updTarget
	}
	return updateTarget{URL: url}
}

func (a *App) beginUpdateDownload(cancel context.CancelFunc) bool {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	if a.updCancel != nil {
		return false
	}
	a.updCancel = cancel
	return true
}

func (a *App) endUpdateDownload() {
	a.updMu.Lock()
	cancel := a.updCancel
	a.updCancel = nil
	a.updMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func updateCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "em-wall", "updates"), nil
}

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// cacheKey names a target's files: its version tag when known, else a
// hash of the URL.
func (t updateTarget) cacheKey() string {
	if k := unsafeKeyChars.ReplaceAllString(t.Version, "_"); k != "" && k != "." && k != ".." {
		return k
	}
	sum := sha256.Sum256([]byte(t.URL))
	return "url-" + hex.EncodeToString(sum[:6])
}

type updatePaths struct{ dmg, part, meta string }

func pathsFor(dir string, t updateTarget) updatePaths {
	base := filepath.Join(dir, t.cacheKey()+".dmg")
	return updatePaths{dmg: base, part: base + ".part", meta: base + ".json"}
}

// cachedUpdateBytes reports how much of t is already on disk: the full
// size for a finished .dmg, the partial length for a resumable one, else 0.
func cachedUpdateBytes(t updateTarget) int64 {
	dir, err := updateCacheDir()
	if err != nil || t.URL == "" {
		return 0
	}
	p := pathsFor(dir, t)
	if fi, err := os.Stat(p.dmg); err == nil {
		if t.Size <= 0 || fi.Size() == t.Size {
			return fi.Size()
		}
		return 0
	}
	if m, ok := loadUpdateMeta(p.meta); !ok || m.URL != t.URL {
		return 0
	}
	if fi, err := os.Stat(p.part); err == nil {
		return fi.Size()
	}
	return 0
}

// discardUpdate removes a downloaded .dmg and its metadata.
func discardUpdate(dmgPath string) {
	_ = os.Remove(dmgPath)
	_ = os.Remove(dmgPath + ".json")
	_ = os.Remove(dmgPath + ".part")
}

// pruneUpdateCache deletes files belonging to any other release.
func pruneUpdateCache(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), keep+".dmg") {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func loadUpdateMeta(path string) (updateMeta, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateMeta{}, false
	}
	var m updateMeta
	if json.Unmarshal(b, &m) != nil {
		return updateMeta{}, false
	}
	return m, true
}

func saveUpdateMeta(path string, m updateMeta) error {
	b, _ := json.Marshal(m)
	return os.WriteFile(path, b, 0o600)
}

// verifyUpdate checks a finished file against the release's digest, or
// its size when no digest was published.
func verifyUpdate(t updateTarget, path string) error {
	if hexSum, ok := strings.CutPrefix(t.Digest, "sha256:"); ok && hexSum != "" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, hexSum) {
			return fmt.Errorf("checksum mismatch (got sha256:%s…)", got[:12])
		}
		return nil
	}
	if t.Size > 0 {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.Size() != t.Size {
			return fmt.Errorf("size mismatch: %d of %d bytes", fi.Size(), t.Size)
		}
	}
	return nil
}

// fetchUpdate returns the path of a complete, verified .dmg for t,
// downloading (or resuming) it as needed.
func (a *App) fetchUpdate(ctx context.Context, t updateTarget) (string, error) {
	dir, err := updateCacheDir()
	if err != nil {
		return "", fmt.Errorf("update: cache dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("update: cache dir: %w", err)
	}
	pruneUpdateCache(dir, t.cacheKey())
	p := pathsFor(dir, t)

	// Already downloaded by an earlier run whose install step failed.
	if _, err := os.Stat(p.dmg); err == nil {
		if m, ok := loadUpdateMeta(p.meta); ok && m.URL == t.URL && verifyUpdate(t, p.dmg) == nil {
			emitEvent(a.ctx, "update:progress", 1.0)
			return p.dmg, nil
		}
		_ = os.Remove(p.dmg)
	}

	corrupt := 0
	for attempt := 1; ; attempt++ {
		err := a.fetchOnce(ctx, t, p)
		if err == nil {
			if verr := verifyUpdate(t, p.part); verr != nil {
				// The bytes on disk are wrong; resuming would keep them.
				_ = os.Remove(p.part)
				_ = os.Remove(p.meta)
				corrupt++
				err = fmt.Errorf("downloaded file failed verification: %w", verr)
				if corrupt >= 2 {
					err = permanentError{err}
				}
			} else if rerr := os.Rename(p.part, p.dmg); rerr != nil {
				return "", fmt.Errorf("update: finalize: %w", rerr)
			} else {
				emitEvent(a.ctx, "update:progress", 1.0)
				return p.dmg, nil
			}
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("download cancelled — %s kept, resume to continue", humanBytes(fileSize(p.part)))
		}
		var perm permanentError
		if errors.As(err, &perm) || attempt >= updateMaxAttempts {
			kept := fileSize(p.part)
			if kept > 0 {
				return "", fmt.Errorf("update: download failed after %d attempt(s): %v — %s kept, resume to continue", attempt, err, humanBytes(kept))
			}
			return "", fmt.Errorf("update: download failed after %d attempt(s): %v", attempt, err)
		}

		wait := backoff(attempt)
		emitEvent(a.ctx, "update:retry", updateRetryEvent{
			Attempt:    attempt,
			Max:        updateMaxAttempts,
			WaitSec:    wait.Seconds(),
			Error:      err.Error(),
			Downloaded: fileSize(p.part),
			Size:       t.Size,
		})
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("download cancelled — %s kept, resume to continue", humanBytes(fileSize(p.part)))
		case <-timer.C:
		}
	}
}

// backoff is exponential with ±20% jitter, capped at updateBackoffMax.
func backoff(attempt int) time.Duration {
	d := updateBackoffBase << (attempt - 1)
	if d <= 0 || d > updateBackoffMax {
		d = updateBackoffMax
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// fetchOnce runs one HTTP request, appending to the partial file from
// where it ends. Returns nil once the partial file holds the whole body.
func (a *App) fetchOnce(ctx context.Context, t updateTarget, p updatePaths) error {
	meta, ok := loadUpdateMeta(p.meta)
	offset := fileSize(p.part)
	// Resume only when the server can confirm the bytes on disk still
	// belong to the same file (If-Range below). Without a strong ETag or
	// Last-Modified a changed file would be silently spliced onto the old
	// prefix, so start over instead.
	if !ok || meta.URL != t.URL || ifRangeValidator(meta) == "" {
		meta = updateMeta{URL: t.URL}
		offset = 0
	}
	// A partial file already as long as the release is complete — the last
	// attempt died between the final byte and returning.
	if t.Size > 0 && offset == t.Size {
		return nil
	}
	if t.Size > 0 && offset > t.Size {
		offset = 0
	}

	// Stall watchdog: the attempt is cancelled when no byte arrives for
	// updateStallTimeout, including while waiting for the response.
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(updateStallTimeout, func() { cancel(errUpdateStalled) })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, t.URL, nil)
	if err != nil {
		return permanentError{fmt.Errorf("request: %w", err)}
	}
	req.Header.Set("User-Agent", "em-wall/"+version.Version)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		// If-Range: when the file changed server-side the server sends the
		// whole new body (200) instead of a range of it.
		req.Header.Set("If-Range", ifRangeValidator(meta))
	}

	resp, err := updateHTTP.Do(req)
	if err != nil {
		return attemptErr(attemptCtx, err)
	}
	defer resp.Body.Close()

	total := t.Size
	flags := os.O_WRONLY | os.O_CREATE
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			// Can't line the range up with the file — start over.
			_ = os.Remove(p.part)
			return fmt.Errorf("server sent an unexpected range (%q)", resp.Header.Get("Content-Range"))
		}
		if size > 0 {
			total = size
		}
		flags |= os.O_APPEND
	case http.StatusOK:
		offset = 0
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		flags |= os.O_TRUNC
	case http.StatusRequestedRangeNotSatisfiable:
		// Our offset is at or past the end. If the size is known and
		// matches, we're done; otherwise drop the partial and refetch.
		if _, size, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && size == offset {
			return nil
		}
		_ = os.Remove(p.part)
		return errors.New("server rejected the resume range; restarting from zero")
	default:
		err := fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return err
		}
		return permanentError{err}
	}

	meta.Size = total
	if v := resp.Header.Get("ETag"); v != "" {
		meta.ETag = v
	}
	if v := resp.Header.Get("Last-Modified"); v != "" {
		meta.LastModified = v
	}
	if err := saveUpdateMeta(p.meta, meta); err != nil {
		return permanentError{fmt.Errorf("write metadata: %w", err)}
	}

	f, err := os.OpenFile(p.part, flags, 0o600)
	if err != nil {
		return permanentError{fmt.Errorf("open partial file: %w", err)}
	}
	pw := &progressWriter{ctx: a.ctx, total: total, written: offset}
	pw.emit(true)
	body := &watchdogReader{r: resp.Body, watchdog: watchdog}
	_, copyErr := io.Copy(io.MultiWriter(f, pw), body)
	// Sync before reporting: the bytes on disk are what the next attempt
	// resumes from.
	syncErr := f.Sync()
	closeErr := f.Close()
	pw.emit(true)
	if copyErr != nil {
		return attemptErr(attemptCtx, copyErr)
	}
	if syncErr != nil {
		return permanentError{fmt.Errorf("write: %w", syncErr)}
	}
	if closeErr != nil {
		return permanentError{fmt.Errorf("write: %w", closeErr)}
	}
	if got := fileSize(p.part); total > 0 && got != total {
		return fmt.Errorf("connection closed early (%s of %s)", humanBytes(got), humanBytes(total))
	}
	return nil
}

// ifRangeValidator picks the If-Range value for a resume: a strong ETag,
// else Last-Modified. A weak ETag (W/"…") is not allowed in If-Range.
func ifRangeValidator(m updateMeta) string {
	if m.ETag != "" && !strings.HasPrefix(m.ETag, "W/") {
		return m.ETag
	}
	return m.LastModified
}

// attemptErr names a stall instead of reporting it as "context canceled".
func attemptErr(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errUpdateStalled) {
		return fmt.Errorf("%w: no data for %s", errUpdateStalled, updateStallTimeout)
	}
	return err
}

// parseContentRange parses "bytes START-END/SIZE" (SIZE may be "*") and
// "bytes */SIZE". size is -1 when unknown.
func parseContentRange(v string) (start, size int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, sz, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	size = -1
	if sz != "*" {
		n, err := strconv.ParseInt(sz, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		size = n
	}
	if rng == "*" {
		return 0, size, true
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return start, size, true
}

// watchdogReader re-arms the stall timer on every read that made progress.
type watchdogReader struct {
	r        io.Reader
	watchdog *time.Timer
}

func (w *watchdogReader) Read(b []byte) (int, error) {
	n, err := w.r.Read(b)
	if n > 0 {
		w.watchdog.Reset(updateStallTimeout)
	}
	return n, err
}

// progressWriter emits "update:progress" (fraction 0..1 of the whole
// file, counting bytes kept from earlier attempts), throttled to whole
// percent changes and updateProgressPeriod.
type progressWriter struct {
	ctx      context.Context
	total    int64
	written  int64
	lastPct  int
	lastEmit time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	p.emit(false)
	return len(b), nil
}

func (p *progressWriter) emit(force bool) {
	if p.total <= 0 {
		return
	}
	pct := int(float64(p.written) / float64(p.total) * 100)
	if !force && (pct == p.lastPct || time.Since(p.lastEmit) < updateProgressPeriod) {
		return
	}
	p.lastPct = pct
	p.lastEmit = time.Now()
	emitEvent(p.ctx, "update:progress", float64(p.written)/float64(p.total))
}

// emitEvent forwards to the Wails runtime, and is a no-op without one
// (tests, or before startup) instead of the runtime's fatal error.
func emitEvent(ctx context.Context, name string, data any) {
	if ctx == nil || ctx.Value("events") == nil {
		return
	}
	runtime.EventsEmit(ctx, name, data)
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
