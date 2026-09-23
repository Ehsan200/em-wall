package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ehsan/em-wall/core/xray"
)

// xrayAPITimeout is the per-call timeout (seconds) handed to `xray api -t`.
const xrayAPITimeout = "5"

// apiServerAddr is the loopback address the running config's api inbound
// listens on (every generated config has one).
func apiServerAddr() string { return "127.0.0.1:" + strconv.Itoa(xray.ApiPort) }

// runAPI shells `xray api <sub> --server=... -t=... [rest...]` and returns
// combined output. Callers may hold s.mu (the call is bounded by -t).
func (s *xraySupervisor) runAPI(ctx context.Context, sub string, rest ...string) ([]byte, error) {
	addr := s.apiAddr
	if addr == "" {
		addr = apiServerAddr()
	}
	args := append([]string{"api", sub, "--server=" + addr, "-t=" + xrayAPITimeout}, rest...)
	cmd := exec.CommandContext(ctx, s.binaryPath, args...)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+s.dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("xray api %s: %w: %s", sub, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// SyncDialerMembers brings slot membership in line with the stores after
// routine node churn (subscription refresh, node enable/disable, cap
// changes). Membership is just outbounds and balancer selectors in the
// generated config, so this is Reconcile — which applies it to the running
// process through the API rather than restarting it.
func (s *xraySupervisor) SyncDialerMembers(ctx context.Context) error {
	return s.Reconcile(ctx)
}

// BalancerInfoRaw returns the raw `xray api bi` output for the loaded slot
// balancers. NOTE: bi requires explicit balancer tags (a no-arg call
// errors), and its output is human-formatted TEXT (not JSON) exposing each
// balancer's current "Selects" (the live winner) — per-node RTT is not
// available via the CLI. The UI parses the winner tags out of the text.
// Empty when no slots run.
func (s *xraySupervisor) BalancerInfoRaw(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	running := s.enabled && s.cmd != nil && len(s.loadedSlots) > 0
	tags := make([]string, 0, len(s.loadedSlots))
	for _, sl := range s.loadedSlots {
		tags = append(tags, xray.SlotBalancerTag(sl.Index))
	}
	s.mu.Unlock()
	if !running || len(tags) == 0 {
		return nil, nil
	}
	return s.runAPI(ctx, "bi", tags...)
}

