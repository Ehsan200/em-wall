package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/xray"
)

// xrayLogFiles are the files xray writes when logDir is non-empty.
// Kept aligned with the log paths in core/xray.Generate.
var xrayLogFiles = []string{"em-wall-xray-access.log", "em-wall-xray-error.log"}

// xrayLogCapBytes is the hard cap on each xray log file. Every
// supervisor restart truncates these files (rewrites them from scratch)
// and a periodic checker rotates by restarting xray when either file
// crosses the cap. The cap is per-file, so the total disk footprint is
// at most 2× this value.
const xrayLogCapBytes = 50 * 1024 * 1024

// xraySupervisor owns the xray-core subprocess and the hidden
// proxy.Proxy rows the dnsproxy/proxytun layer uses to dial each
// entry's local SOCKS5 inbound.
//
// "Disabled" mode: when the xray binary isn't on disk (dev builds,
// failed install) the supervisor is constructed but never starts a
// subprocess. Reconcile() still syncs hidden proxy rows from the
// xray store so existing rules don't reference dangling names —
// they'll just fail to dial until the binary appears.
// xrayRecentLineCap is how many recent xray stdout/stderr lines the
// supervisor retains for the UI's diagnostic panel. Big enough to
// surface a startup crash with a few lines of context, small enough
// to stay in-process without log-file pressure.
const xrayRecentLineCap = 80

type xraySupervisor struct {
	binaryPath string
	dataDir    string // contains geoip.dat + geosite.dat
	runtimeDir string // generated config + scratch
	logDir     string // where xray writes its own access/error logs
	xrayStore  *xray.Store
	proxyStore *proxy.Store
	logger     *log.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd     // current child process, nil if none running
	waitCh   chan struct{} // closed by the watcher goroutine when cmd exits
	enabled  bool          // binary present at construction time
	version  string        // xray version string, captured once at startup
	tail     *xrayLineRing // last N stdout/stderr lines, surfaced via RecentLines
	lastExit string        // most recent unexpected exit description (empty until one happens)
}

// newXraySupervisor probes the binary and returns a supervisor in
// either enabled or disabled mode. Either way callers can invoke
// Reconcile/Stop/Version without nil checks.
func newXraySupervisor(binary, dataDir, runtimeDir, logDir string, xs *xray.Store, ps *proxy.Store, logger *log.Logger) *xraySupervisor {
	sup := &xraySupervisor{
		binaryPath: binary,
		dataDir:    dataDir,
		runtimeDir: runtimeDir,
		logDir:     logDir,
		xrayStore:  xs,
		proxyStore: ps,
		logger:     logger,
		tail:       newXrayLineRing(xrayRecentLineCap),
	}
	if fi, err := os.Stat(binary); err == nil && !fi.IsDir() {
		sup.enabled = true
		if v, err := xray.Version(binary); err == nil {
			sup.version = v
			logger.Printf("xray supervisor: %s", v)
		}
	} else {
		logger.Printf("xray supervisor: binary %s not found — xray:NAME rules will fail to dial", binary)
	}
	return sup
}

// Enabled reports whether the binary was on disk at construction —
// the UI uses this to decide whether to gray out the Xray panel.
func (s *xraySupervisor) Enabled() bool { return s.enabled }

// Version returns the cached `xray version` first line (empty when
// disabled or the probe failed).
func (s *xraySupervisor) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Running reports whether the xray subprocess is currently alive.
// False when the supervisor is disabled, hasn't started one yet, or
// the last process exited (crash or graceful).
func (s *xraySupervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// RecentLines returns the most recent xray stdout/stderr lines (up
// to xrayRecentLineCap), oldest first. Surfaces startup crashes to
// the UI without making the user open the system log.
func (s *xraySupervisor) RecentLines() []string {
	if s.tail == nil {
		return nil
	}
	return s.tail.Snapshot()
}

// LastExit returns a one-line description of the most recent
// unexpected exit (empty until one happens). Useful when xray
// crashed and Running()==false — pairs with RecentLines() for the
// "why".
func (s *xraySupervisor) LastExit() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastExit
}

// Reconcile syncs hidden proxy rows, regenerates the xray config, and
// (re)starts the subprocess if there are enabled entries. Idempotent;
// safe to call after every Add/Update/Delete IPC. Always syncs the
// proxy rows even when disabled — so that switching modes (binary
// arrives later, user toggles a row) doesn't leave dangling state.
func (s *xraySupervisor) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.xrayStore.List(ctx)
	if err != nil {
		return fmt.Errorf("xray supervisor: list: %w", err)
	}

	if err := s.syncProxies(ctx, entries); err != nil {
		return err
	}

	if !s.enabled {
		return nil
	}

	hasEnabled := false
	for _, e := range entries {
		if e.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		s.stopLocked()
		return nil
	}

	routing, err := s.xrayStore.GetRoutingRules(ctx)
	if err != nil {
		return fmt.Errorf("xray supervisor: get routing rules: %w", err)
	}
	cfg, err := xray.Generate(entries, xray.GenerateOptions{
		LogDir:       s.logDir,
		RoutingRules: routing,
	})
	if err != nil {
		return fmt.Errorf("xray supervisor: generate: %w", err)
	}
	if err := os.MkdirAll(s.runtimeDir, 0o755); err != nil {
		return fmt.Errorf("xray supervisor: mkdir runtime: %w", err)
	}
	cfgPath := filepath.Join(s.runtimeDir, "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		return fmt.Errorf("xray supervisor: write config: %w", err)
	}

	return s.restartLocked(cfgPath)
}

// syncProxies aligns hidden proxy.Proxy rows (_xray_*) with the
// current set of enabled xray entries: add missing rows, update ports
// that drifted, remove rows for entries the user deleted.
func (s *xraySupervisor) syncProxies(ctx context.Context, entries []xray.Config) error {
	want := make(map[string]proxy.Proxy, len(entries))
	for _, e := range entries {
		// Disabled entries still get a proxy row so a rule referencing
		// them resolves successfully at decision time — the dial will
		// fail (nothing listening on the port) and the user sees a
		// connection error, which is the right signal that they
		// disabled it on purpose.
		want[xray.InternalProxyName(e.Name)] = proxy.Proxy{
			Name:     xray.InternalProxyName(e.Name),
			Protocol: proxy.ProtocolSOCKS5,
			Host:     "127.0.0.1",
			Port:     e.SocksPort,
		}
	}

	existing, err := s.proxyStore.List(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]proxy.Proxy)
	for _, p := range existing {
		if xray.IsInternalProxyName(p.Name) {
			have[p.Name] = p
		}
	}

	for name, w := range want {
		cur, ok := have[name]
		if !ok {
			if _, err := s.proxyStore.Add(ctx, w); err != nil && !errors.Is(err, proxy.ErrDuplicate) {
				return fmt.Errorf("xray supervisor: add hidden proxy %q: %w", name, err)
			}
			continue
		}
		if cur.Port != w.Port || cur.Host != w.Host || cur.Protocol != w.Protocol {
			w.ID = cur.ID
			if err := s.proxyStore.Update(ctx, w); err != nil {
				return fmt.Errorf("xray supervisor: update hidden proxy %q: %w", name, err)
			}
		}
	}
	for name, p := range have {
		if _, keep := want[name]; !keep {
			if err := s.proxyStore.Delete(ctx, p.ID); err != nil {
				return fmt.Errorf("xray supervisor: delete hidden proxy %q: %w", name, err)
			}
		}
	}
	return nil
}

// restartLocked stops any running xray process, truncates the log
// files (so each restart starts from a clean slate — explicit user
// requirement), then starts a fresh xray against cfgPath. Caller
// holds s.mu.
func (s *xraySupervisor) restartLocked(cfgPath string) error {
	s.stopLocked()
	s.truncateLogsLocked()

	cmd := exec.Command(s.binaryPath, "run", "-config", cfgPath)
	cmd.Env = append(os.Environ(),
		"XRAY_LOCATION_ASSET="+s.dataDir,
	)
	cmd.Stdout = &xrayLogWriter{logger: s.logger, prefix: "xray: ", ring: s.tail}
	cmd.Stderr = cmd.Stdout
	// New process group so signals we send don't accidentally hit any
	// caller-spawned siblings.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("xray supervisor: start: %w", err)
	}

	done := make(chan struct{})
	s.cmd = cmd
	s.waitCh = done

	go func(c *exec.Cmd, done chan struct{}) {
		err := c.Wait()
		close(done)
		s.mu.Lock()
		defer s.mu.Unlock()
		// Only log as unexpected if stopLocked hasn't already cleared
		// s.cmd — otherwise this exit is the one stopLocked asked for.
		if s.cmd == c {
			msg := "process exited unexpectedly"
			if err != nil {
				msg += ": " + err.Error()
			}
			s.logger.Printf("xray supervisor: %s", msg)
			s.lastExit = msg
			s.cmd = nil
			s.waitCh = nil
		}
	}(cmd, done)

	s.logger.Printf("xray supervisor: started, pid=%d, config=%s", cmd.Process.Pid, cfgPath)
	return nil
}

// stopLocked sends SIGTERM, waits up to 2s, then SIGKILL. Caller
// holds s.mu. No-op if nothing is running.
func (s *xraySupervisor) stopLocked() {
	if s.cmd == nil {
		return
	}
	cmd, done := s.cmd, s.waitCh
	s.cmd = nil
	s.waitCh = nil

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	s.logger.Printf("xray supervisor: stopped pid=%d", cmd.Process.Pid)
}

// Stop tears down the subprocess. Hidden proxy rows are intentionally
// NOT cleared — they're persistent state, not runtime state, so a
// daemon restart picks them up via Reconcile.
func (s *xraySupervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// truncateLogsLocked rewrites each xray log file to zero bytes.
// Called from restartLocked so every restart starts logging from
// scratch (user requirement: "if I restart xray it should rewrite
// that file again"). xray opens its log files with O_APPEND, so
// writes after this truncation correctly resume at offset 0.
//
// Missing files are not an error — the first start hasn't created
// them yet, or the log dir was wiped between runs.
func (s *xraySupervisor) truncateLogsLocked() {
	if s.logDir == "" {
		return
	}
	for _, name := range xrayLogFiles {
		p := filepath.Join(s.logDir, name)
		if err := os.Truncate(p, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.logger.Printf("xray supervisor: truncate %s: %v", p, err)
		}
	}
}

// RotateLogsIfTooLarge restarts xray when either log file has crossed
// xrayLogCapBytes — restart truncates as a side-effect. Called
// periodically from main.go's watcher goroutine. No-op when disabled,
// when no logs are configured, or when nothing is currently running.
func (s *xraySupervisor) RotateLogsIfTooLarge() {
	if !s.enabled || s.logDir == "" {
		return
	}
	over := false
	for _, name := range xrayLogFiles {
		fi, err := os.Stat(filepath.Join(s.logDir, name))
		if err == nil && fi.Size() > xrayLogCapBytes {
			over = true
			break
		}
	}
	if !over {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		// Not running — just truncate in place; next start will be
		// fresh anyway, but doing this now prevents the cap-exceeded
		// state from lingering on disk.
		s.truncateLogsLocked()
		return
	}
	cfgPath := filepath.Join(s.runtimeDir, "config.json")
	s.logger.Printf("xray supervisor: log files exceeded %d bytes — rotating", xrayLogCapBytes)
	if err := s.restartLocked(cfgPath); err != nil {
		s.logger.Printf("xray supervisor: rotate-restart failed: %v", err)
	}
}

// xrayLogWriter splits xray's stdout/stderr into lines, re-prints
// each through the daemon logger with a prefix, and (when ring is
// non-nil) appends the raw line to a ring buffer so the UI can show
// recent output without scraping the daemon log.
type xrayLogWriter struct {
	logger *log.Logger
	prefix string
	ring   *xrayLineRing
	mu     sync.Mutex
	buf    []byte
}

var _ io.Writer = (*xrayLogWriter)(nil)

func (w *xrayLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.logger.Printf("%s%s", w.prefix, line)
		if w.ring != nil {
			w.ring.Push(line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// xrayLineRing is a small fixed-capacity ring buffer of strings,
// safe for one writer (the xrayLogWriter) and many concurrent
// snapshot readers (the IPC handler). Snapshot copies, so callers
// don't need to hold any lock.
type xrayLineRing struct {
	mu   sync.Mutex
	buf  []string
	cap  int
	head int // index of the next slot to write
	full bool
}

func newXrayLineRing(capacity int) *xrayLineRing {
	if capacity <= 0 {
		capacity = 1
	}
	return &xrayLineRing{cap: capacity, buf: make([]string, capacity)}
}

func (r *xrayLineRing) Push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = line
	r.head = (r.head + 1) % r.cap
	if r.head == 0 {
		r.full = true
	}
}

// Snapshot returns the buffered lines oldest-first.
func (r *xrayLineRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]string, r.head)
		copy(out, r.buf[:r.head])
		return out
	}
	out := make([]string, 0, r.cap)
	out = append(out, r.buf[r.head:]...)
	out = append(out, r.buf[:r.head]...)
	return out
}
