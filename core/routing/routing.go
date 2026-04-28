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
	"strconv"
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
	host      string
	iface     string
	expiresAt time.Time
	ruleID    int64
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

// detectVPNOwners labels tunnel interfaces with their owner. Strategy
// (in priority order, later sources only fill gaps):
//
//  1. lsof: every process with an open `utun_control` ctl-socket has
//     a "unit" number that maps directly to the utun. Most accurate
//     when this code runs as root (which the daemon does).
//
//  2. SystemConfiguration via scutil — picks up NEPacketTunnelProvider
//     VPNs registered as a Network Service.
//
//  3. Process-name scan — last-ditch hint for known VPN binaries; can
//     only label "some VPN is running", not which utun is which.
func detectVPNOwners() map[string]string {
	out := map[string]string{}

	// 1. lsof: per-utun owning process.
	for iface, proc := range lsofUtunOwners() {
		out[iface] = proc
	}

	// 2. SystemConfiguration mapping — overlays a friendlier name on
	// top of (or alongside) the process name.
	for iface, name := range scutilServiceMapping() {
		if existing, ok := out[iface]; ok && existing != name {
			out[iface] = name + " — " + existing
		} else {
			out[iface] = name
		}
	}

	// 2. Process-scan fallback for tunnels not covered above.
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
	if containsAny("v2box", "v2ray", "v2rayn") {
		hints = append(hints, "v2box")
	}
	if containsAny("clash", "clashx", "clashverge", "stash") {
		hints = append(hints, "Clash")
	}
	if containsAny("shadowsocks") {
		hints = append(hints, "Shadowsocks")
	}

	if len(hints) > 0 {
		label := strings.Join(hints, " / ")
		ifs, _ := net.Interfaces()
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagUp == 0 {
				continue
			}
			if !isTunnelName(ifc.Name) {
				continue
			}
			if _, already := out[ifc.Name]; already {
				continue
			}
			out[ifc.Name] = label + " (process-scan)"
		}
	}
	return out
}

// lsofUtunOwners returns iface→process-name for every utun that has
// a known owning process. Walks `lsof -nP` output and matches lines
// with `[ctl com.apple.net.utun_control id <id> unit <unit>]` — the
// `unit` number maps to utun(unit-1). Requires root for full
// visibility; the daemon runs as root so this is fine in production.
func lsofUtunOwners() map[string]string {
	out := map[string]string{}
	cmd := exec.Command("/usr/sbin/lsof", "-nP")
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	re := regexp.MustCompile(`(\S+)\s+\d+\s.*\[ctl com\.apple\.net\.utun_control id \d+ unit (\d+)\]`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		proc := m[1]
		unit, err := strconv.Atoi(m[2])
		if err != nil || unit < 1 {
			continue
		}
		ifname := fmt.Sprintf("utun%d", unit-1)
		// Don't overwrite — keep the first process seen so multiple fd
		// holders on the same tunnel don't churn the label.
		if _, ok := out[ifname]; !ok {
			out[ifname] = proc
		}
	}
	return out
}

// scutilServiceMapping returns iface→service-name for every network
// service macOS knows about that has bound to a tunnel interface.
// One scutil invocation feeds a small batch script; we then parse it
// once.
func scutilServiceMapping() map[string]string {
	out := map[string]string{}

	// Step 1: list all service IPv4 keys.
	listCmd := exec.Command("/usr/sbin/scutil")
	listCmd.Stdin = strings.NewReader("list State:/Network/Service/.*/IPv4\nquit\n")
	listOut, err := listCmd.Output()
	if err != nil {
		return out
	}
	keyRe := regexp.MustCompile(`State:/Network/Service/([0-9A-Fa-f-]+)/IPv4`)
	var uuids []string
	seen := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(string(listOut), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			uuids = append(uuids, m[1])
		}
	}

	// Step 2: for each UUID, fetch InterfaceName from State and
	// UserDefinedName from Setup. Bundle into one scutil script.
	if len(uuids) == 0 {
		return out
	}
	var script strings.Builder
	for _, u := range uuids {
		script.WriteString("show State:/Network/Service/" + u + "/IPv4\n")
		script.WriteString("show Setup:/Network/Service/" + u + "\n")
	}
	script.WriteString("quit\n")
	showCmd := exec.Command("/usr/sbin/scutil")
	showCmd.Stdin = strings.NewReader(script.String())
	showOut, err := showCmd.Output()
	if err != nil {
		return out
	}

	// Parse: each "show" produces a block separated by "<dictionary>"
	// or by another show. We split on lines and walk.
	blocks := splitScutilBlocks(string(showOut))
	// blocks[i*2] is State (has InterfaceName), blocks[i*2+1] is Setup
	// (has UserDefinedName). Bail gracefully if counts don't match.
	for i, u := range uuids {
		if 2*i+1 >= len(blocks) {
			break
		}
		ifname := scutilField(blocks[2*i], "InterfaceName")
		if ifname == "" || !isTunnelName(ifname) {
			continue
		}
		name := scutilField(blocks[2*i+1], "UserDefinedName")
		if name == "" {
			name = scutilField(blocks[2*i], "ServiceID")
		}
		if name == "" {
			name = u[:8]
		}
		out[ifname] = name
	}
	return out
}

// splitScutilBlocks separates the scutil output of multiple `show`
// commands into individual top-level dictionary blocks. scutil
// dictionaries can nest (e.g. AdditionalRoutes embeds a dictionary),
// so we track brace depth instead of splitting on "<dictionary>".
func splitScutilBlocks(s string) []string {
	var blocks []string
	var cur strings.Builder
	depth := 0
	started := false
	for _, ch := range s {
		if started {
			cur.WriteRune(ch)
		}
		switch ch {
		case '{':
			if !started {
				started = true
				cur.WriteRune(ch)
			}
			depth++
		case '}':
			depth--
			if started && depth == 0 {
				blocks = append(blocks, cur.String())
				cur.Reset()
				started = false
			}
		}
	}
	return blocks
}

func scutilField(block, field string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\s*:\s*(.+?)\s*$`)
	m := re.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return m[1]
}

func isTunnelName(name string) bool {
	return strings.HasPrefix(name, "utun") ||
		strings.HasPrefix(name, "ipsec") ||
		strings.HasPrefix(name, "ppp") ||
		strings.HasPrefix(name, "tun") ||
		strings.HasPrefix(name, "tap")
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
