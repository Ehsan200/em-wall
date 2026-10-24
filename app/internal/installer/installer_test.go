package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/geo/meta"
)

// checkBash runs `bash -n` (parse, don't execute) over a generated
// script. These scripts run as root via osascript, so a syntax error
// would surface as a half-finished install on a user's machine.
func checkBash(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("/bin/bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid bash: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

func TestInstallScriptInstallsCLI(t *testing.T) {
	script := installScript("/tmp/em-walld", "/tmp/em-wall", "/tmp/plist", "/tmp/anchor",
		"/tmp/xray", "/tmp/geoip.dat", "/tmp/geosite.dat")
	checkBash(t, script)

	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Errorf("script does not start with a shebang:\n%s", firstLines(script, 3))
	}
	want := "install -m 0755 " + strconv.Quote("/tmp/em-wall") + " " + strconv.Quote(CLIBinaryDest)
	if !strings.Contains(script, want) {
		t.Errorf("missing CLI install line %q", want)
	}
	// The daemon must still be installed — the CLI line is inserted next
	// to it, not in place of it.
	if !strings.Contains(script, strconv.Quote(DaemonBinaryDest)) {
		t.Errorf("daemon install line went missing")
	}
}

// A resources/ dir staged by an older Makefile has no em-wall binary.
// Install must still produce a working script.
func TestInstallScriptWithoutCLI(t *testing.T) {
	script := installScript("/tmp/em-walld", "", "/tmp/plist", "/tmp/anchor",
		"/tmp/xray", "/tmp/geoip.dat", "/tmp/geosite.dat")
	checkBash(t, script)

	// Quoted so this doesn't match the em-walld destination, which has
	// the CLI's path as a prefix.
	if strings.Contains(script, strconv.Quote(CLIBinaryDest)) {
		t.Errorf("CLI referenced despite an empty cliPath:\n%s", script)
	}
	if !strings.Contains(script, strconv.Quote(DaemonBinaryDest)) {
		t.Errorf("daemon install line went missing")
	}
}

func TestUninstallScriptRemovesCLI(t *testing.T) {
	for _, purge := range []bool{false, true} {
		script := uninstallScript(purge)
		checkBash(t, script)

		if !strings.Contains(script, "CLI_BIN_DST="+strconv.Quote(CLIBinaryDest)) {
			t.Errorf("purge=%v: CLI destination not bound", purge)
		}
		if !strings.Contains(script, `"$CLI_BIN_DST"`) {
			t.Errorf("purge=%v: CLI never removed", purge)
		}
		if purge != strings.Contains(script, strconv.Quote(DBDir)) {
			t.Errorf("purge=%v: DB removal block is wrong", purge)
		}
	}
}

// An empty geo path keeps the installed file; a set one installs it.
func TestInstallScriptGeoFiles(t *testing.T) {
	both := installScript("/tmp/em-walld", "/tmp/em-wall", "/tmp/plist", "/tmp/anchor",
		"/tmp/xray", "/tmp/geoip.dat", "/tmp/geosite.dat")
	checkBash(t, both)
	for _, want := range []string{
		"install -m 0644 " + strconv.Quote("/tmp/geoip.dat") + ` "$XRAY_GEOIP"`,
		"install -m 0644 " + strconv.Quote("/tmp/geosite.dat") + ` "$XRAY_GEOSITE"`,
	} {
		if !strings.Contains(both, want) {
			t.Errorf("missing %q", want)
		}
	}
	none := installScript("/tmp/em-walld", "/tmp/em-wall", "/tmp/plist", "/tmp/anchor",
		"/tmp/xray", "", "")
	checkBash(t, none)
	if strings.Contains(none, `"$XRAY_GEOIP"`+"\n") || strings.Contains(none, `install -m 0644 "" `) ||
		strings.Contains(none, ` "$XRAY_GEOSITE"`+"\n") {
		t.Errorf("kept geo files still installed:\n%s", none)
	}
	if !strings.Contains(none, "launchctl bootstrap") {
		t.Error("script truncated")
	}
}

func TestKeepDownloadedGeo(t *testing.T) {
	bundled := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if s, i := keepDownloadedGeo(dir, bundled); s || i {
		t.Error("no geo.meta: bundle must go in")
	}
	for _, f := range []string{"geosite.dat", "geoip.dat"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := meta.Write(dir, meta.Meta{
		meta.GeoSite: {Source: "loyalsoldier", UpdatedAt: bundled.Add(48 * time.Hour)},  // newer download
		meta.GeoIP:   {Source: "loyalsoldier", UpdatedAt: bundled.Add(-48 * time.Hour)}, // older than the bundle
	})
	if err != nil {
		t.Fatal(err)
	}
	if s, i := keepDownloadedGeo(dir, bundled); !s || i {
		t.Errorf("keep = %v,%v; want geosite kept, geoip replaced", s, i)
	}
	_ = meta.Write(dir, meta.Meta{meta.GeoSite: {Source: "bundled", UpdatedAt: bundled.Add(48 * time.Hour)}})
	if s, _ := keepDownloadedGeo(dir, bundled); s {
		t.Error("a bundled entry was kept over the bundle")
	}
	_ = meta.Write(dir, meta.Meta{meta.GeoSite: {Source: "loyalsoldier", UpdatedAt: bundled.Add(48 * time.Hour)}})
	_ = os.Remove(filepath.Join(dir, "geosite.dat"))
	if s, _ := keepDownloadedGeo(dir, bundled); s {
		t.Error("kept a file that is not there")
	}
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
