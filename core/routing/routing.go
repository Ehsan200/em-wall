// Package routing installs and removes per-host routes that pin a
// destination IP to a specific egress interface. On macOS this calls
// `route -n add -host <ip> -interface <iface>` (and -inet6 for v6).
package routing

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Runner abstracts shelling out to /sbin/route. The default ExecRunner
// runs the real binary; tests inject a recorder.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type entry struct {
	host       string
	iface      string
	expiresAt  time.Time
	ruleID     int64
}

// Manager owns the set of active host routes. Safe for concurrent use.
type Manager struct {
	runner Runner
	mu     sync.Mutex
	routes map[string]entry // key: host
	now    func() time.Time
}

func New(runner Runner) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{
		runner: runner,
		routes: make(map[string]entry),
		now:    time.Now,
	}
}

// Install pins host to iface for ttl duration. Re-installing the same
// host with a different interface replaces the route.
func (m *Manager) Install(ctx context.Context, host, iface string, ttl time.Duration, ruleID int64) error {
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("routing: invalid IP %q", host)
	}
	if iface == "" {
		return fmt.Errorf("routing: empty interface")
	}

	m.mu.Lock()
	prev, had := m.routes[host]
	m.mu.Unlock()

	if had && prev.iface != iface {
		_ = m.removeOne(ctx, host, ip.To4() != nil)
	}

	args := []string{"-n", "add"}
	if ip.To4() == nil {
		args = append(args, "-inet6")
	}
	args = append(args, "-host", host, "-interface", iface)
	out, err := m.runner.Run(ctx, "/sbin/route", args...)
	if err != nil && !looksLikeAlreadyExists(out) {
		return fmt.Errorf("route add %s via %s: %w (%s)", host, iface, err, out)
	}

	m.mu.Lock()
	m.routes[host] = entry{
		host:      host,
		iface:     iface,
		expiresAt: m.now().Add(ttl),
		ruleID:    ruleID,
	}
	m.mu.Unlock()
	return nil
}

// Remove drops the route for a single host.
func (m *Manager) Remove(ctx context.Context, host string) error {
	m.mu.Lock()
	_, ok := m.routes[host]
	if ok {
		delete(m.routes, host)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	ip := net.ParseIP(host)
	return m.removeOne(ctx, host, ip != nil && ip.To4() != nil)
}

// RemoveByRule flushes every route that was installed on behalf of ruleID.
func (m *Manager) RemoveByRule(ctx context.Context, ruleID int64) error {
	m.mu.Lock()
	var hosts []string
	for h, e := range m.routes {
		if e.ruleID == ruleID {
			hosts = append(hosts, h)
		}
	}
	m.mu.Unlock()
	var firstErr error
	for _, h := range hosts {
		if err := m.Remove(ctx, h); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SweepExpired removes routes whose TTL has passed.
func (m *Manager) SweepExpired(ctx context.Context) int {
	now := m.now()
	m.mu.Lock()
	var stale []string
	for h, e := range m.routes {
		if now.After(e.expiresAt) {
			stale = append(stale, h)
		}
	}
	m.mu.Unlock()
	for _, h := range stale {
		_ = m.Remove(ctx, h)
	}
	return len(stale)
}

// Flush removes every route the manager owns. Call on shutdown.
func (m *Manager) Flush(ctx context.Context) {
	m.mu.Lock()
	hosts := make([]string, 0, len(m.routes))
	for h := range m.routes {
		hosts = append(hosts, h)
	}
	m.mu.Unlock()
	for _, h := range hosts {
		_ = m.Remove(ctx, h)
	}
}

// Active returns a snapshot of current routes (for the UI).
func (m *Manager) Active() []ActiveRoute {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ActiveRoute, 0, len(m.routes))
	for _, e := range m.routes {
		out = append(out, ActiveRoute{
			Host:      e.host,
			Interface: e.iface,
			ExpiresAt: e.expiresAt,
			RuleID:    e.ruleID,
		})
	}
	return out
}

type ActiveRoute struct {
	Host      string
	Interface string
	ExpiresAt time.Time
	RuleID    int64
}

func (m *Manager) removeOne(ctx context.Context, host string, isV4 bool) error {
	args := []string{"-n", "delete"}
	if !isV4 {
		args = append(args, "-inet6")
	}
	args = append(args, "-host", host)
	out, err := m.runner.Run(ctx, "/sbin/route", args...)
	if err != nil && !looksLikeNotFound(out) {
		return fmt.Errorf("route delete %s: %w (%s)", host, err, out)
	}
	return nil
}

func looksLikeAlreadyExists(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "File exists") || strings.Contains(s, "already in table")
}

func looksLikeNotFound(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "not in table") || strings.Contains(s, "No such process")
}

// EnumerateInterfaces returns active non-loopback interfaces, useful
// for the UI's interface picker.
func EnumerateInterfaces() ([]Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	owners := detectVPNOwners()
	var out []Interface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		out = append(out, Interface{
			Name:  ifc.Name,
			Index: ifc.Index,
			MTU:   ifc.MTU,
			Flags: ifc.Flags.String(),
			Owner: owners[ifc.Name],
		})
	}
	return out, nil
}

type Interface struct {
	Name  string
	Index int
	MTU   int
	Flags string
	Owner string // best-effort label, e.g. "Tailscale", "WireGuard", "VPN: Work"
}

// detectVPNOwners is a best-effort labeller for tunnel interfaces.
// macOS does not expose which process owns a utun, so we infer from
// the set of running VPN-related processes and from `scutil --nc list`
// for built-in VPNs. The same hint is applied to every up tunnel
// interface — good enough for the user to identify what they're
// looking at.
func detectVPNOwners() map[string]string {
	out := map[string]string{}

	psOut, _ := exec.Command("/bin/ps", "-axo", "comm").Output()
	procs := strings.ToLower(string(psOut))
	containsAny := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(procs, n) {
				return true
			}
		}
		return false
	}

	var hints []string
	if containsAny("tailscaled", "tailscale.app") {
		hints = append(hints, "Tailscale")
	}
	if containsAny("wireguard-go", "wg-quick", "wireguard.app") {
		hints = append(hints, "WireGuard")
	}
	if containsAny("openvpn", "tunnelblick", "viscosity") {
		hints = append(hints, "OpenVPN")
	}
	if containsAny("warp-svc", "cloudflarewarp") {
		hints = append(hints, "Cloudflare WARP")
	}
	if containsAny("nordvpn") {
		hints = append(hints, "NordVPN")
	}
	if containsAny("expressvpn") {
		hints = append(hints, "ExpressVPN")
	}
	if containsAny("protonvpn") {
		hints = append(hints, "ProtonVPN")
	}
	if containsAny("mullvad") {
		hints = append(hints, "Mullvad")
	}
	if containsAny("globalprotect") {
		hints = append(hints, "GlobalProtect")
	}
	if containsAny("anyconnect", "cisco_secure_client") {
		hints = append(hints, "Cisco AnyConnect")
	}

	// Built-in macOS VPNs (PPP/IPSec/IKEv2) via scutil.
	ncOut, _ := exec.Command("/usr/sbin/scutil", "--nc", "list").Output()
	ncRe := regexp.MustCompile(`\(Connected\)\s+\S+\s+\S+\s+"([^"]+)"`)
	for _, m := range ncRe.FindAllStringSubmatch(string(ncOut), -1) {
		hints = append(hints, "VPN: "+m[1])
	}

	if len(hints) == 0 {
		return out
	}
	label := strings.Join(hints, " / ")

	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		name := ifc.Name
		if strings.HasPrefix(name, "utun") ||
			strings.HasPrefix(name, "ipsec") ||
			strings.HasPrefix(name, "ppp") ||
			strings.HasPrefix(name, "tun") ||
			strings.HasPrefix(name, "tap") {
			out[name] = label
		}
	}
	return out
}

// SystemRoute is one row of the OS routing table (parsed from
// `netstat -rn`).
type SystemRoute struct {
	Family      string // "inet" or "inet6"
	Destination string
	Gateway     string
	Flags       string
	Interface   string
}

// ListSystemRoutes returns the IPv4 and IPv6 routing table snapshots.
// Used by the UI to show what egress paths the OS currently has —
// e.g. a new utun route appearing when a VPN connects.
func ListSystemRoutes() ([]SystemRoute, error) {
	out, err := exec.Command("/usr/sbin/netstat", "-rn").Output()
	if err != nil {
		return nil, fmt.Errorf("netstat -rn: %w", err)
	}
	return parseNetstat(out), nil
}

// parseNetstat understands the BSD netstat -rn layout used on macOS.
// Sections are introduced by "Internet:" / "Internet6:" lines, then
// a column header, then rows.
func parseNetstat(b []byte) []SystemRoute {
	var out []SystemRoute
	family := ""
	header := false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			header = false
			continue
		}
		switch {
		case strings.HasPrefix(line, "Internet6:"):
			family = "inet6"
			header = false
			continue
		case strings.HasPrefix(line, "Internet:"):
			family = "inet"
			header = false
			continue
		case strings.HasPrefix(line, "Routing tables"):
			continue
		}
		if !header {
			if strings.HasPrefix(strings.TrimSpace(line), "Destination") {
				header = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		out = append(out, SystemRoute{
			Family:      family,
			Destination: fields[0],
			Gateway:     fields[1],
			Flags:       fields[2],
			Interface:   fields[3],
		})
	}
	return out
}
