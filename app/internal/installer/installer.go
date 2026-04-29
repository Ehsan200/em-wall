package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotPackaged means the embedded resources are missing — the app
// was likely built via plain `wails dev` instead of `make app-bundle`,
// so the daemon binary was never copied into resources/. Install can't
// proceed, but the UI can still talk to a separately-running daemon.
var ErrNotPackaged = errors.New("daemon resources not embedded — build with `make app-bundle`")

// IsPackaged reports whether the app binary contains a usable daemon.
// Callers should hide install-related UI when this is false.
func IsPackaged() bool {
	fi, err := resources.Open("resources/em-walld")
	if err != nil {
		return false
	}
	defer fi.Close()
	stat, err := fi.Stat()
	if err != nil {
		return false
	}
	return stat.Size() > 0
}

// Filesystem destinations. Kept in lock-step with launchd/com.em-wall.daemon.plist
// and core/ipc.DefaultSocketPath. Changing one means changing all.
const (
	DaemonBinaryDest = "/usr/local/bin/em-walld"
	PlistDest        = "/Library/LaunchDaemons/com.em-wall.daemon.plist"
	AnchorFile       = "/etc/pf.anchors/em-wall"
	PFConf           = "/etc/pf.conf"
	SocketPath       = "/var/run/em-wall.sock"
	DBDir            = "/usr/local/var/em-wall"
	DBFile           = "/usr/local/var/em-wall/rules.db"
	LogDir           = "/usr/local/var/log"
	LogFile          = "/usr/local/var/log/em-wall.log"
	LaunchctlLabel   = "com.em-wall.daemon"
)

// Status is a snapshot of what's currently on disk and whether the
// daemon is running. Cheap to compute — the UI polls it.
type Status struct {
	BinaryPresent bool  `json:"binaryPresent"`
	PlistPresent  bool  `json:"plistPresent"`
	PFConfPatched bool  `json:"pfConfPatched"`
	SocketPresent bool  `json:"socketPresent"`
	DaemonRunning bool  `json:"daemonRunning"`
	DBExists      bool  `json:"dbExists"`
	DBSizeBytes   int64 `json:"dbSizeBytes"`
	LogSizeBytes  int64 `json:"logSizeBytes"`
}

// FullyInstalled is the green-light condition: every artefact in place
// and the LaunchDaemon is running. UI uses this to decide between the
// install gate and the regular tab UI.
func (s Status) FullyInstalled() bool {
	return s.BinaryPresent && s.PlistPresent && s.DaemonRunning
}

// Probe inspects the filesystem and launchctl. No escalation needed —
// these are all read-only checks that work for an unprivileged user.
// `launchctl print system/<label>` does require sudo on some macOS
// versions; we fall back to the socket check if it errors out.
func Probe(ctx context.Context) Status {
	s := Status{}
	if fi, err := os.Stat(DaemonBinaryDest); err == nil && !fi.IsDir() {
		s.BinaryPresent = true
	}
	if _, err := os.Stat(PlistDest); err == nil {
		s.PlistPresent = true
	}
	if data, err := os.ReadFile(PFConf); err == nil {
		s.PFConfPatched = strings.Contains(string(data), `anchor "em-wall"`)
	}
	if _, err := os.Stat(SocketPath); err == nil {
		s.SocketPresent = true
	}
	if fi, err := os.Stat(DBFile); err == nil {
		s.DBExists = true
		s.DBSizeBytes = fi.Size()
	}
	if fi, err := os.Stat(LogFile); err == nil {
		s.LogSizeBytes = fi.Size()
	}

	// `launchctl print` works without root in modern macOS for system
	// targets the user can see. Treat any non-error output as truth.
	if out, err := exec.CommandContext(ctx,
		"/bin/launchctl", "print", "system/"+LaunchctlLabel).Output(); err == nil {
		s.DaemonRunning = strings.Contains(string(out), "state = running")
	} else {
		// Fallback: if the socket exists and answers, daemon is up.
		s.DaemonRunning = s.SocketPresent
	}
	return s
}

// Install lays down the embedded resources into a temp dir and runs
// the privileged install script via osascript admin escalation.
// Blocks until the script finishes. The temp dir is removed on return.
func Install(ctx context.Context) error {
	if !IsPackaged() {
		return ErrNotPackaged
	}

	tmp, err := os.MkdirTemp("", "em-wall-install-")
	if err != nil {
		return fmt.Errorf("install: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	binPath := filepath.Join(tmp, "em-walld")
	plistPath := filepath.Join(tmp, "com.em-wall.daemon.plist")
	anchorPath := filepath.Join(tmp, "em-wall.pf.anchor")
	scriptPath := filepath.Join(tmp, "install.sh")

	if err := extract("resources/em-walld", binPath, 0o755); err != nil {
		return fmt.Errorf("install: extract binary: %w", err)
	}
	if err := extract("resources/com.em-wall.daemon.plist", plistPath, 0o644); err != nil {
		return fmt.Errorf("install: extract plist: %w", err)
	}
	if err := extract("resources/em-wall.pf.anchor", anchorPath, 0o644); err != nil {
		return fmt.Errorf("install: extract anchor: %w", err)
	}
	if err := os.WriteFile(scriptPath, []byte(installScript(binPath, plistPath, anchorPath)), 0o700); err != nil {
		return fmt.Errorf("install: write script: %w", err)
	}
	return runWithAdminPrivileges(ctx, scriptPath)
}

// extract reads name from the embedded FS and writes it to dst with
// the given mode. Used to materialize the daemon binary, plist, and
// anchor stub before the install script copies them into place.
func extract(name, dst string, mode os.FileMode) error {
	data, err := resources.ReadFile(name)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

// Uninstall stops the daemon and removes installed files. If purge is
// true, the SQLite DB and log file are removed too. If false, they
// stay so a future re-install picks up where the user left off.
//
// The caller (UI) MUST refuse to run this while the system DNS hijack
// is active — otherwise removing the daemon while 127.0.0.1 is in
// every service's DNS list means everything breaks.
func Uninstall(ctx context.Context, purge bool) error {
	tmp, err := os.MkdirTemp("", "em-wall-uninstall-")
	if err != nil {
		return fmt.Errorf("uninstall: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	scriptPath := filepath.Join(tmp, "uninstall.sh")
	if err := os.WriteFile(scriptPath, []byte(uninstallScript(purge)), 0o700); err != nil {
		return fmt.Errorf("uninstall: write script: %w", err)
	}
	return runWithAdminPrivileges(ctx, scriptPath)
}

// installScript builds the privileged install bash payload. Inputs
// are paths to the temp-extracted daemon binary, plist, and pf anchor
// stub — the script just `install`s them into their final locations,
// patches /etc/pf.conf to load the anchor, then bootstraps the
// LaunchDaemon. Idempotent — safe to re-run.
func installScript(binPath, plistPath, anchorPath string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

DAEMON_BIN_DST=%q
PLIST_DST=%q
ANCHOR_FILE=%q
PF_CONF=%q
LOG_DIR=%q
DB_DIR=%q

install -m 0755 %q "$DAEMON_BIN_DST"

mkdir -p "$DB_DIR" "$LOG_DIR"
chmod 0755 "$DB_DIR" "$LOG_DIR"

mkdir -p "$(dirname "$ANCHOR_FILE")"
[ -e "$ANCHOR_FILE" ] || install -m 0644 %q "$ANCHOR_FILE"
chmod 0644 "$ANCHOR_FILE"

if ! grep -q '^anchor "em-wall"' "$PF_CONF"; then
    cp "$PF_CONF" "$PF_CONF.em-wall.bak.$(date +%%s)"
    sed -i.tmp '/em-wall: anchors for DNS hijack/d;/em-wall: encrypted DNS blocking anchor/d;/^rdr-anchor "em-wall"$/d;/^anchor "em-wall"$/d;/^load anchor "em-wall" from /d' "$PF_CONF"
    rm -f "$PF_CONF.tmp"
    {
        echo ''
        echo '# em-wall: encrypted DNS blocking anchor'
        echo 'anchor "em-wall"'
        echo 'load anchor "em-wall" from "/etc/pf.anchors/em-wall"'
    } >> "$PF_CONF"
fi

pfctl -e 2>/dev/null || true
pfctl -f "$PF_CONF" 2>/dev/null || true

install -m 0644 -o root -g wheel %q "$PLIST_DST"

launchctl bootout system "$PLIST_DST" 2>/dev/null || true
launchctl bootstrap system "$PLIST_DST"
launchctl enable system/com.em-wall.daemon
launchctl kickstart -k system/com.em-wall.daemon
`,
		DaemonBinaryDest, PlistDest, AnchorFile, PFConf, LogDir, DBDir,
		binPath, anchorPath, plistPath,
	)
}

// uninstallScript builds the privileged uninstall bash payload: stops
// the daemon, removes installed files, strips em-wall lines from
// /etc/pf.conf, and (when purge) deletes the rules DB and log file.
//
// The script ends with a safety sweep — every network service whose
// first DNS entry is still 127.0.0.1 gets reset to DHCP-supplied. The
// UI also asks the daemon to deactivate the hijack via IPC before this
// script runs (that path uses the daemon's saved per-service backup to
// restore the *original* DNS, not just DHCP), but that's best-effort.
// The sweep is the last line of defence against leaving the host with
// broken DNS if the deactivate IPC failed (daemon crashed, backup lost,
// etc.).
func uninstallScript(purge bool) string {
	purgeBlock := ""
	if purge {
		purgeBlock = fmt.Sprintf("rm -rf %q %q\n", DBDir, LogFile)
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

PLIST_DST=%q
DAEMON_BIN_DST=%q
ANCHOR_FILE=%q
PF_CONF=%q
SOCKET=%q

launchctl bootout system "$PLIST_DST" 2>/dev/null || true
pfctl -a em-wall -F all 2>/dev/null || true

rm -f "$PLIST_DST" "$DAEMON_BIN_DST" "$SOCKET" "$ANCHOR_FILE"

if grep -qE '^(rdr-)?anchor "em-wall"' "$PF_CONF"; then
    cp "$PF_CONF" "$PF_CONF.em-wall.uninstall.$(date +%%s)"
    sed -i.tmp '/em-wall: anchors for DNS hijack/d;/em-wall: encrypted DNS blocking anchor/d;/^rdr-anchor "em-wall"$/d;/^anchor "em-wall"$/d;/^load anchor "em-wall" from /d' "$PF_CONF"
    rm -f "$PF_CONF.tmp"
    pfctl -f "$PF_CONF" || true
fi

# Safety sweep: if the daemon's deactivate didn't run (or didn't fully
# restore), every network service still pointing at 127.0.0.1 gets
# reset to DHCP-supplied DNS so the machine isn't stranded.
networksetup -listallnetworkservices | tail -n +2 | while IFS= read -r svc; do
    case "$svc" in \**) continue ;; esac  # skip disabled services (prefixed with "*")
    if networksetup -getdnsservers "$svc" 2>/dev/null | head -1 | grep -q '^127\.0\.0\.1$'; then
        echo "em-wall: resetting DNS for $svc (was 127.0.0.1)"
        networksetup -setdnsservers "$svc" empty || true
    fi
done

dscacheutil -flushcache 2>/dev/null || true
killall -HUP mDNSResponder 2>/dev/null || true

%s`,
		PlistDest, DaemonBinaryDest, AnchorFile, PFConf, SocketPath,
		purgeBlock,
	)
}

// runWithAdminPrivileges runs the bash script via osascript's "do
// shell script ... with administrator privileges". macOS shows the
// standard authorization prompt; if the user clicks Cancel, osascript
// exits non-zero with "(-128)" in stderr — surface that as a typed
// error so the UI can show a friendly message instead of a scary one.
//
// scriptPath is a path produced by os.MkdirTemp + filepath.Join, so it
// can't contain shell metacharacters. We single-quote it to be safe.
func runWithAdminPrivileges(ctx context.Context, scriptPath string) error {
	osa := fmt.Sprintf(`do shell script "/bin/bash '%s' 2>&1" with administrator privileges`,
		scriptPath)
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", osa).CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		if strings.Contains(s, "(-128)") || strings.Contains(s, "User canceled") {
			return ErrCancelled
		}
		if s == "" {
			return fmt.Errorf("escalation failed: %w", err)
		}
		return fmt.Errorf("escalation failed: %s", s)
	}
	return nil
}

// ErrCancelled is returned when the user dismisses the macOS auth
// prompt. The UI should treat this as a non-error.
var ErrCancelled = errors.New("user cancelled the authorization prompt")

// IsCancelled reports whether err is the user-cancelled-prompt error.
func IsCancelled(err error) bool { return errors.Is(err, ErrCancelled) }
