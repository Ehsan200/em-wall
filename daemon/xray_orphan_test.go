package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSweepOrphanXray verifies the startup sweep kills a stale
// process that matches the supervisor's binary name. The fake binary
// is deliberately NOT named "em-wall-xray" so running the suite on a
// machine with a live daemon can't take down its real child.
func TestSweepOrphanXray(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "em-wall-xray-testfake")

	src, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skipf("no /bin/sleep to copy: %v", err)
	}
	if err := os.WriteFile(fake, src, 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// Setpgid mirrors restartLocked, so this stands in for a real orphan.
	cmd := exec.Command(fake, "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake orphan: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap, so the pid doesn't linger as a zombie

	// pgrep reads the process table; give the exec a moment to land.
	waitFor(t, func() bool { return syscall.Kill(pid, 0) == nil }, "orphan to be running")

	sweepOrphanXray(fake, log.New(io.Discard, "", 0))

	waitFor(t, func() bool { return syscall.Kill(pid, 0) != nil }, "orphan to be killed")
}

// TestSweepOrphanXrayNoMatch is the healthy path: nothing matching is
// running, so the sweep must be a quiet no-op rather than an error or
// a panic on pgrep's exit status 1.
func TestSweepOrphanXrayNoMatch(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "em-wall-xray-testfake-absent")
	sweepOrphanXray(fake, log.New(io.Discard, "", 0))
}

// TestSweepOrphanXraySkipsSelf guards the pid==self filter: passing
// the running test binary's own path must not kill the test process.
func TestSweepOrphanXraySkipsSelf(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path: %v", err)
	}
	sweepOrphanXray(self, log.New(io.Discard, "", 0))
	// Reaching this line at all is the assertion.
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
