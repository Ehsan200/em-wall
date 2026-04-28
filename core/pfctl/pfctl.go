// Package pfctl manages a small pf anchor that blocks encrypted DNS
// (DoT on TCP/853 and DoH to a curated set of well-known endpoints).
//
// One-time install (done by scripts/install.sh) must add this to
// /etc/pf.conf:
//   anchor "em-wall"
//   load anchor "em-wall" from "/etc/pf.anchors/em-wall"
// and ensure pf is enabled (`pfctl -e`). At runtime this package
// rewrites the anchor's rules and reloads them via `pfctl -a em-wall`.
package pfctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const AnchorName = "em-wall"

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunStdin(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (ExecRunner) RunStdin(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.CombinedOutput()
}

type Manager struct {
	runner Runner

	mu      sync.Mutex
	enabled bool
}

func New(runner Runner) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{runner: runner}
}

func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// Enable loads the block rules into our pf anchor.
func (m *Manager) Enable(ctx context.Context) error {
	rules := blockRules()
	out, err := m.runner.RunStdin(ctx, []byte(rules), "/sbin/pfctl", "-a", AnchorName, "-f", "-")
	if err != nil {
		return fmt.Errorf("pfctl load anchor: %w (%s)", err, trim(out))
	}
	if looksLikeAnchorMissing(out) {
		return errors.New("pfctl: anchor 'em-wall' not declared in /etc/pf.conf — run scripts/install.sh first")
	}
	m.mu.Lock()
	m.enabled = true
	m.mu.Unlock()
	return nil
}

// Disable flushes our anchor, leaving it empty.
func (m *Manager) Disable(ctx context.Context) error {
	out, err := m.runner.Run(ctx, "/sbin/pfctl", "-a", AnchorName, "-F", "all")
	if err != nil {
		return fmt.Errorf("pfctl flush anchor: %w (%s)", err, trim(out))
	}
	m.mu.Lock()
	m.enabled = false
	m.mu.Unlock()
	return nil
}

// Sync drives the anchor toward the desired state.
func (m *Manager) Sync(ctx context.Context, desiredOn bool) error {
	if desiredOn {
		return m.Enable(ctx)
	}
	return m.Disable(ctx)
}

// blockRules returns the pf rule body. Drops:
//   - all outbound TCP to *:853 (DoT)
//   - outbound TCP/443 to a curated list of DoH endpoint IPs
//
// Plain UDP/53 to these hosts is left alone so the daemon can still
// use them as upstream resolvers.
func blockRules() string {
	v4 := []string{
		"1.1.1.1", "1.0.0.1",
		"8.8.8.8", "8.8.4.4",
		"9.9.9.9", "149.112.112.112",
	}
	v6 := []string{
		"2606:4700:4700::1111", "2606:4700:4700::1001",
		"2001:4860:4860::8888", "2001:4860:4860::8844",
		"2620:fe::fe", "2620:fe::9",
	}
	return strings.Join([]string{
		"# em-wall: block encrypted DNS",
		"block drop out quick proto tcp to any port 853",
		"block drop out quick proto tcp to { " + strings.Join(v4, " ") + " } port 443",
		"block drop out quick proto tcp to { " + strings.Join(v6, " ") + " } port 443",
		"",
	}, "\n")
}

func trim(b []byte) string { return strings.TrimSpace(string(b)) }

func looksLikeAnchorMissing(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "anchor not found") || strings.Contains(s, "no such anchor")
}
