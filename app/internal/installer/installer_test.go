package installer

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
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

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
