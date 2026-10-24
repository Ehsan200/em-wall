package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
)

func geoStatusFixture(updating bool, siteErr string) ipc.GeoStatusResult {
	return ipc.GeoStatusResult{
		Files: []ipc.GeoFileDTO{
			{File: "geosite", Source: "loyalsoldier", Present: true, Version: "202610230000", SizeBytes: 10 << 20,
				Entries: 1429, HasBackup: true, UpdatedAt: "2026-10-23T22:13:00Z", LastError: siteErr},
			{File: "city", Source: "dbip"},
		},
		GeositeSource: "loyalsoldier", AutoUpdate: true, Updating: updating,
	}
}

func TestGeoStatus(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGeoStatus: func(json.RawMessage) (any, error) { return geoStatusFixture(false, ""), nil },
	})
	code, out, _ := runCLI(s.sock, "geo")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"geosite", "202610230000", "10.0 MB", "1429 tags", "missing", "direct only"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// update returns at once with Updating set; the CLI polls status until
// the daemon is done, then exits by the asked-for file's error.
func TestGeoUpdateWait(t *testing.T) {
	old := geoPollInterval
	geoPollInterval = 5 * time.Millisecond
	defer func() { geoPollInterval = old }()

	var polls atomic.Int32
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGeoUpdate: func(json.RawMessage) (any, error) { return geoStatusFixture(true, ""), nil },
		ipc.MethodGeoStatus: func(json.RawMessage) (any, error) {
			if polls.Add(1) < 3 {
				return geoStatusFixture(true, ""), nil
			}
			return geoStatusFixture(false, "checksum mismatch"), nil
		},
	})
	code, out, _ := runCLI(s.sock, "geo", "update", "geosite")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(out, "checksum mismatch") || polls.Load() < 3 {
		t.Errorf("polls=%d output:\n%s", polls.Load(), out)
	}
	var p ipc.GeoUpdateParams
	decodeParams(t, s.params(t, ipc.MethodGeoUpdate), &p)
	if p.File != "geosite" {
		t.Errorf("params = %+v", p)
	}

	// An error on a file that wasn't asked for doesn't fail "update city".
	code, _, _ = runCLI(s.sock, "geo", "update", "city", "--no-wait")
	if code != exitOK {
		t.Errorf("--no-wait exit = %d", code)
	}
}

func TestGeoUsage(t *testing.T) {
	s := newStub(t, nil)
	if code, _, _ := runCLI(s.sock, "geo", "update", "nope"); code != exitUsage {
		t.Errorf("bad file exit = %d", code)
	}
	if code, _, _ := runCLI(s.sock, "geo", "rollback", "x"); code != exitUsage {
		t.Errorf("rollback arg exit = %d", code)
	}
}
