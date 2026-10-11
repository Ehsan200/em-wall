package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testPayload() []byte {
	return bytes.Repeat([]byte("em-wall-update-"), 20000) // 300 KB
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func useTempCache(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	updateBackoffBase = 5 * time.Millisecond
	updateStallTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		updateBackoffBase = time.Second
		updateStallTimeout = 30 * time.Second
	})
}

// serveFlaky serves payload with Range support; the first `drops`
// requests are cut off after `cut` bytes of body.
func serveFlaky(payload []byte, drops int32, cut int) (*httptest.Server, *atomic.Int32, *[]string) {
	var reqs atomic.Int32
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqs.Add(1)
		ranges = append(ranges, r.Header.Get("Range"))
		if n <= drops {
			// Announce the full (remaining) body, send a slice, hang up.
			w.Header().Set("ETag", `"v1"`)
			start := 0
			if rng := r.Header.Get("Range"); rng != "" {
				start, _, _ = parseRangeStart(rng)
				w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(payload)-1)+"/"+strconv.Itoa(len(payload)))
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.WriteHeader(http.StatusOK)
			}
			_, _ = w.Write(payload[start:min(start+cut, len(payload))])
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "u.dmg", time.Time{}, bytes.NewReader(payload))
	}))
	return srv, &reqs, &ranges
}

func parseRangeStart(h string) (int, int, bool) {
	s, _, _ := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	n, err := strconv.Atoi(s)
	return n, 0, err == nil
}

func TestFetchUpdateResumesAfterDrops(t *testing.T) {
	useTempCache(t)
	payload := testPayload()
	srv, reqs, ranges := serveFlaky(payload, 2, 50_000)
	defer srv.Close()

	a := &App{}
	tg := updateTarget{Version: "v9.9.9", URL: srv.URL + "/u.dmg", Size: int64(len(payload)), Digest: digestOf(payload)}

	// First drop is on a fresh request; but the drop writes a 200 with a
	// bogus length, so the client sees an early close and resumes.
	path, err := a.fetchUpdate(context.Background(), tg)
	if err != nil {
		t.Fatalf("fetchUpdate: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: %d vs %d bytes", len(got), len(payload))
	}
	if reqs.Load() != 3 {
		t.Fatalf("requests = %d, want 3", reqs.Load())
	}
	if (*ranges)[1] == "" || (*ranges)[2] == "" {
		t.Fatalf("retries did not resume with Range: %q", *ranges)
	}

	// A second call reuses the verified file without any request.
	before := reqs.Load()
	if _, err := a.fetchUpdate(context.Background(), tg); err != nil {
		t.Fatalf("second fetchUpdate: %v", err)
	}
	if reqs.Load() != before {
		t.Fatalf("cached .dmg was downloaded again")
	}
	if cachedUpdateBytes(tg) != int64(len(payload)) {
		t.Fatalf("cachedUpdateBytes = %d", cachedUpdateBytes(tg))
	}
}

func TestFetchUpdateKeepsPartialAcrossCalls(t *testing.T) {
	useTempCache(t)
	payload := testPayload()
	// More drops than one call's attempts: the call fails, keeping bytes.
	srv, _, _ := serveFlaky(payload, updateMaxAttempts, 10_000)
	defer srv.Close()

	a := &App{}
	tg := updateTarget{Version: "v9.9.9", URL: srv.URL + "/u.dmg", Size: int64(len(payload)), Digest: digestOf(payload)}
	_, err := a.fetchUpdate(context.Background(), tg)
	if err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("want failure mentioning kept bytes, got %v", err)
	}
	kept := cachedUpdateBytes(tg)
	if kept != int64(updateMaxAttempts*10_000) {
		t.Fatalf("kept = %d, want %d", kept, updateMaxAttempts*10_000)
	}

	// Next call (server healthy now) finishes from where it stopped.
	path, err := a.fetchUpdate(context.Background(), tg)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("resumed payload mismatch")
	}
}

func TestFetchUpdateStallIsRetried(t *testing.T) {
	useTempCache(t)
	payload := testPayload()
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("Content-Length", "999999")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:1000])
			w.(http.Flusher).Flush()
			select { // stall
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		http.ServeContent(w, r, "u.dmg", time.Time{}, bytes.NewReader(payload))
	}))
	defer srv.Close()

	a := &App{}
	tg := updateTarget{Version: "v1", URL: srv.URL, Size: int64(len(payload))}
	if _, err := a.fetchUpdate(context.Background(), tg); err != nil {
		t.Fatalf("fetchUpdate: %v", err)
	}
}

func TestFetchUpdateNotFoundIsPermanent(t *testing.T) {
	useTempCache(t)
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	a := &App{}
	if _, err := a.fetchUpdate(context.Background(), updateTarget{Version: "v1", URL: srv.URL}); err == nil {
		t.Fatal("want error")
	}
	if reqs.Load() != 1 {
		t.Fatalf("404 retried: %d requests", reqs.Load())
	}
}

func TestFetchUpdateBadDigest(t *testing.T) {
	useTempCache(t)
	payload := testPayload()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "u.dmg", time.Time{}, bytes.NewReader(payload))
	}))
	defer srv.Close()
	a := &App{}
	tg := updateTarget{Version: "v1", URL: srv.URL, Digest: digestOf([]byte("other"))}
	if _, err := a.fetchUpdate(context.Background(), tg); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("want verification error, got %v", err)
	}
	if cachedUpdateBytes(tg) != 0 {
		t.Fatal("corrupt download left in cache")
	}
}

// A partial file whose server gave no validator must not be resumed:
// nothing proves the remaining bytes belong to the same file.
func TestFetchUpdateNoValidatorRestarts(t *testing.T) {
	useTempCache(t)
	payload := testPayload()
	var ranges []string
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if reqs.Add(1) == 1 {
			w.Header().Set("Content-Length", "999999")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:5000])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	a := &App{}
	tg := updateTarget{Version: "v1", URL: srv.URL, Size: int64(len(payload)), Digest: digestOf(payload)}
	if _, err := a.fetchUpdate(context.Background(), tg); err != nil {
		t.Fatalf("fetchUpdate: %v", err)
	}
	for _, r := range ranges {
		if r != "" {
			t.Fatalf("resumed without a validator: %q", ranges)
		}
	}
}

// A file changed on the server between attempts (If-Range mismatch → 200)
// must replace the partial, not be appended to it.
func TestFetchUpdateChangedFileRestarts(t *testing.T) {
	useTempCache(t)
	oldPayload := bytes.Repeat([]byte("old-"), 50000)
	payload := testPayload()
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("ETag", `"old"`)
			w.Header().Set("Content-Length", "999999")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oldPayload[:5000])
			return
		}
		w.Header().Set("ETag", `"new"`)
		http.ServeContent(w, r, "u.dmg", time.Time{}, bytes.NewReader(payload))
	}))
	defer srv.Close()
	a := &App{}
	tg := updateTarget{Version: "v1", URL: srv.URL, Size: int64(len(payload)), Digest: digestOf(payload)}
	path, err := a.fetchUpdate(context.Background(), tg)
	if err != nil {
		t.Fatalf("fetchUpdate: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("old prefix spliced into new file")
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in          string
		start, size int64
		ok          bool
	}{
		{"bytes 100-199/1000", 100, 1000, true},
		{"bytes 0-9/*", 0, -1, true},
		{"bytes */500", 0, 500, true},
		{"items 0-1/2", 0, 0, false},
		{"bytes x-1/2", 0, 0, false},
	}
	for _, c := range cases {
		s, z, ok := parseContentRange(c.in)
		if ok != c.ok || (ok && (s != c.start || z != c.size)) {
			t.Errorf("%q → %d,%d,%v", c.in, s, z, ok)
		}
	}
}
