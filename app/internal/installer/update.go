package installer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// AppBundlePath returns the absolute path of the running .app bundle by
// walking up from the executable until it hits a ".app" directory
// (…/Em Wall.app/Contents/MacOS/Em Wall → …/Em Wall.app). Returns an
// error for a non-bundled binary (e.g. `wails dev`, `go run`), where
// self-update doesn't apply.
func AppBundlePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	for p := exe; p != "/" && p != "."; p = filepath.Dir(p) {
		if strings.HasSuffix(p, ".app") {
			return p, nil
		}
	}
	return "", fmt.Errorf("not running from a .app bundle (%s)", exe)
}

// ApplyAppUpdate mounts the given .dmg, finds the .app inside it, and
// hands off to a detached swap script that replaces the running bundle
// in place once this process exits. The caller MUST quit the app right
// after this returns nil — the script waits on the current PID before
// touching the bundle.
//
// The swap is non-destructive: the old bundle is moved aside first and
// only deleted after the copy succeeds, and the quarantine xattr is
// cleared on the new copy so the unsigned relaunch isn't blocked by
// Gatekeeper. No admin escalation — a drag-installed bundle is owned by
// the user. The matching daemon update is a separate step: the new app
// reports a different version than the running daemon, which surfaces
// the existing "reinstall daemon" banner.
func ApplyAppUpdate(ctx context.Context, dmgPath string) error {
	dst, err := AppBundlePath()
	if err != nil {
		return err
	}

	device, mountPoint, err := attachDMG(ctx, dmgPath)
	if err != nil {
		return err
	}
	// On any failure before handoff, detach so we don't leak a mount.
	detach := func() { _ = exec.Command("/usr/bin/hdiutil", "detach", device, "-force").Run() }

	src, err := findAppInDir(mountPoint)
	if err != nil {
		detach()
		return err
	}

	tmp, err := os.MkdirTemp("", "em-wall-update-")
	if err != nil {
		detach()
		return fmt.Errorf("update: temp dir: %w", err)
	}
	scriptPath := filepath.Join(tmp, "swap.sh")
	if err := os.WriteFile(scriptPath, []byte(swapScript(os.Getpid(), src, dst, device)), 0o700); err != nil {
		detach()
		return fmt.Errorf("update: write script: %w", err)
	}

	// Launch detached so it outlives this process (it waits for us to
	// exit before swapping). New process group → not killed with us.
	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		detach()
		return fmt.Errorf("update: launch swap script: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// attachDMG mounts the dmg with -nobrowse and returns its /dev/diskN
// device node and mount point. Parses the tab-separated `hdiutil attach`
// output: the last whitespace-run-delimited field of the line that has a
// mount point is the mount point; the first field is the device.
func attachDMG(ctx context.Context, dmgPath string) (device, mountPoint string, err error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "attach",
		"-nobrowse", "-noverify", "-noautoopen", dmgPath).Output()
	if err != nil {
		return "", "", fmt.Errorf("update: mount dmg: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "/dev/") {
			continue
		}
		// Format: "<dev>\t<type>\t<mountpoint>", but the mount point can
		// contain spaces. Device is the first field; mount point is
		// everything after the "/Volumes" marker.
		fields := strings.Fields(line)
		dev := fields[0]
		if idx := strings.Index(line, "/Volumes/"); idx >= 0 {
			return dev, strings.TrimRight(line[idx:], " \t"), nil
		}
		// Remember the device even if this row has no mount point.
		device = dev
	}
	if device != "" {
		_ = exec.Command("/usr/bin/hdiutil", "detach", device, "-force").Run()
	}
	return "", "", fmt.Errorf("update: no mounted volume in hdiutil output")
}

// findAppInDir returns the first *.app inside dir.
func findAppInDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("update: read mount: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("update: no .app found in dmg")
}

// swapScript waits for the current app (pid) to exit, replaces the
// destination bundle with the one from the mounted dmg, clears the
// quarantine flag, detaches the dmg, and relaunches. Old bundle is kept
// as .old until the copy succeeds so a failed copy isn't fatal.
func swapScript(pid int, src, dst, device string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -uo pipefail

PID=%d
SRC=%q
DST=%q
DEVICE=%q

# Wait (up to ~30s) for the running app to quit before swapping.
for _ in $(seq 1 100); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.3
done

cleanup() { /usr/bin/hdiutil detach "$DEVICE" -force >/dev/null 2>&1 || true; }

rm -rf "$DST.old"
if [ -d "$DST" ]; then
    mv "$DST" "$DST.old" || { cleanup; exit 1; }
fi
if ! cp -R "$SRC" "$DST"; then
    # Restore the previous bundle on copy failure.
    rm -rf "$DST"
    [ -d "$DST.old" ] && mv "$DST.old" "$DST"
    cleanup
    /usr/bin/open "$DST" >/dev/null 2>&1 || true
    exit 1
fi

xattr -dr com.apple.quarantine "$DST" >/dev/null 2>&1 || true
rm -rf "$DST.old"
cleanup
/usr/bin/open "$DST" >/dev/null 2>&1 || true
`,
		pid, src, dst, device,
	)
}
