package main

// system_dns.go — read and manipulate macOS DNS settings.
//
// Two layers exposed here:
//   - GetServiceDNS / SetServiceDNS — what `networksetup` reports for a
//     given service (Wi-Fi, Ethernet, …). What you'd see in System Settings.
//   - DetectResolvers / dhcpDNS — what the kernel actually uses, including
//     DHCP-supplied resolvers that don't show up in networksetup.
//
// All shell-outs go through Runner so unit tests don't touch the system.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// SystemDNS owns the wiring between macOS network services and the daemon.
type SystemDNS struct {
	r Runner
	// resolverDir is where per-domain split-DNS shadow files are written
	// (/etc/resolver in production). Overridable so tests don't touch the
	// real system path or need root.
	resolverDir string
}

func NewSystemDNS(r Runner) *SystemDNS {
	if r == nil {
		r = execRunner{}
	}
	return &SystemDNS{r: r, resolverDir: "/etc/resolver"}
}

// ListServices returns enabled network service names as they appear in
// `networksetup -listallnetworkservices`. Disabled (asterisk-prefixed)
// services are skipped.
func (s *SystemDNS) ListServices() ([]string, error) {
	out, err := s.r.Run("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, fmt.Errorf("listallnetworkservices: %w (%s)", err, string(out))
	}
	var services []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false
			continue // first line is a header
		}
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

// GetServiceDNS returns the DNS servers configured for a service, in order.
// Returns nil (no error) when no manual DNS is set (DHCP-supplied).
func (s *SystemDNS) GetServiceDNS(service string) ([]string, error) {
	out, err := s.r.Run("networksetup", "-getdnsservers", service)
	if err != nil {
		return nil, fmt.Errorf("getdnsservers %q: %w (%s)", service, err, string(out))
	}
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "aren't any DNS Servers") {
		return nil, nil
	}
	var ips []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ips = append(ips, line)
		}
	}
	return ips, nil
}

// SetServiceDNS replaces the DNS servers for a service. An empty slice
// reverts to DHCP-supplied resolvers (`networksetup … Empty`).
func (s *SystemDNS) SetServiceDNS(service string, servers []string) error {
	args := []string{"-setdnsservers", service}
	if len(servers) == 0 {
		args = append(args, "Empty")
	} else {
		args = append(args, servers...)
	}
	out, err := s.r.Run("networksetup", args...)
	if err != nil {
		return fmt.Errorf("setdnsservers %q: %w (%s)", service, err, string(out))
	}
	return nil
}

// DetectResolvers returns the resolvers the kernel currently uses,
// excluding loopback. Pulled from `scutil --dns`. This catches both
// manually-set and DHCP-supplied servers, so it's our best source for
// auto-populating the daemon's upstream.
func (s *SystemDNS) DetectResolvers() ([]string, error) {
	out, err := s.r.Run("scutil", "--dns")
	if err != nil {
		return nil, fmt.Errorf("scutil --dns: %w (%s)", err, string(out))
	}
	re := regexp.MustCompile(`nameserver\[\d+\]\s*:\s*(\S+)`)
	seen := make(map[string]bool)
	var ips []string
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		ip := m[1]
		if isLoopback(ip) {
			continue
		}
		if seen[ip] {
			continue
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	return ips, nil
}

// PrimaryInterface returns the interface carrying the default IPv4
// route (e.g. "en0"). Used as a fallback to find DHCP-supplied DNS.
func (s *SystemDNS) PrimaryInterface() (string, error) {
	out, err := s.r.Run("route", "-n", "get", "default")
	if err != nil {
		return "", fmt.Errorf("route get default: %w (%s)", err, string(out))
	}
	re := regexp.MustCompile(`(?m)^\s*interface:\s*(\S+)`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("no default route found")
	}
	return m[1], nil
}

// DHCPDNS returns the DHCP-supplied DNS for the primary interface, if
// any. May return nil when not on DHCP (e.g. static IP, manual DNS,
// or when the primary interface is a VPN tunnel).
func (s *SystemDNS) DHCPDNS() ([]string, error) {
	iface, err := s.PrimaryInterface()
	if err != nil {
		return nil, err
	}
	if isTunnelIface(iface) {
		return nil, nil
	}
	return s.dhcpDNSFor(iface)
}

func (s *SystemDNS) dhcpDNSFor(iface string) ([]string, error) {
	out, err := s.r.Run("ipconfig", "getoption", iface, "domain_name_server")
	if err != nil {
		return nil, fmt.Errorf("ipconfig getoption %s: %w (%s)", iface, err, string(out))
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return nil, nil
	}
	return []string{ip}, nil
}

// AllDHCPDNS scans every enabled non-tunnel hardware port and returns
// the union of DHCP-supplied DNS servers. This catches the Wi-Fi
// router DNS even when a VPN owns the default route (which would
// otherwise hide it from PrimaryInterface).
func (s *SystemDNS) AllDHCPDNS() ([]string, error) {
	out, err := s.r.Run("networksetup", "-listallhardwareports")
	if err != nil {
		return nil, fmt.Errorf("listallhardwareports: %w (%s)", err, string(out))
	}
	deviceRe := regexp.MustCompile(`(?m)^Device:\s*(\S+)\s*$`)
	matches := deviceRe.FindAllStringSubmatch(string(out), -1)
	seen := map[string]bool{}
	var servers []string
	for _, m := range matches {
		dev := m[1]
		if isTunnelIface(dev) {
			continue
		}
		ips, err := s.dhcpDNSFor(dev)
		if err != nil || len(ips) == 0 {
			continue
		}
		for _, ip := range ips {
			if isLoopback(ip) || seen[ip] {
				continue
			}
			seen[ip] = true
			servers = append(servers, ip)
		}
	}
	return servers, nil
}

func isTunnelIface(name string) bool {
	return strings.HasPrefix(name, "utun") ||
		strings.HasPrefix(name, "ipsec") ||
		strings.HasPrefix(name, "ppp") ||
		strings.HasPrefix(name, "tun") ||
		strings.HasPrefix(name, "tap")
}

// ValidateResolver sends a real query to addr ("host:port") and reports
// whether it answered successfully. Used to filter dead candidates so
// we never silently use a resolver that can't reach the network.
func ValidateResolver(ctx context.Context, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	c := &dns.Client{Net: "udp", Timeout: 1500 * time.Millisecond}
	m := new(dns.Msg)
	m.SetQuestion("apple.com.", dns.TypeA)
	resp, _, err := c.ExchangeContext(dctx, m, addr)
	if err != nil || resp == nil {
		return false
	}
	if resp.Rcode != dns.RcodeSuccess {
		return false
	}
	for _, rr := range resp.Answer {
		if _, ok := rr.(*dns.A); ok {
			return true
		}
	}
	return false
}

// ValidateResolvers tests every candidate concurrently and returns
// only the ones that answered. Order is preserved relative to input.
func ValidateResolvers(ctx context.Context, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	results := make([]bool, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			results[i] = ValidateResolver(ctx, addr)
		}(i, c)
	}
	wg.Wait()
	out := make([]string, 0, len(candidates))
	for i, c := range candidates {
		if results[i] {
			out = append(out, c)
		}
	}
	return out
}

// CaptureAll snapshots current per-service manual DNS settings. Pass
// the result to RestoreAll later.
func (s *SystemDNS) CaptureAll() (map[string][]string, error) {
	services, err := s.ListServices()
	if err != nil {
		return nil, err
	}
	snap := make(map[string][]string, len(services))
	for _, svc := range services {
		ips, err := s.GetServiceDNS(svc)
		if err != nil {
			continue
		}
		snap[svc] = ips
	}
	return snap, nil
}

// RestoreAll re-applies a prior snapshot. Services not in the snapshot
// are left alone.
func (s *SystemDNS) RestoreAll(snap map[string][]string) error {
	var firstErr error
	for service, ips := range snap {
		if err := s.SetServiceDNS(service, ips); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ApplyAll sets every enabled service's DNS to the given servers.
func (s *SystemDNS) ApplyAll(servers []string) error {
	services, err := s.ListServices()
	if err != nil {
		return err
	}
	var firstErr error
	for _, svc := range services {
		if err := s.SetServiceDNS(svc, servers); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// IsActive reports whether the system is currently routing DNS through
// us (any service has 127.0.0.1 in its resolver list).
func (s *SystemDNS) IsActive() (bool, error) {
	services, err := s.ListServices()
	if err != nil {
		return false, err
	}
	for _, svc := range services {
		ips, _ := s.GetServiceDNS(svc)
		for _, ip := range ips {
			if ip == "127.0.0.1" {
				return true, nil
			}
		}
	}
	return false, nil
}

// PickUpstream chooses what the daemon should forward to. Order:
//  1. Caller-supplied snapshot (the per-service manual values seen
//     before activation), excluding loopback.
//  2. DHCP-supplied resolvers for the primary interface.
//  3. nil — caller decides.
func (s *SystemDNS) PickUpstream(snap map[string][]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, ips := range snap {
		for _, ip := range ips {
			if isLoopback(ip) || seen[ip] {
				continue
			}
			seen[ip] = true
			out = append(out, ip)
		}
	}
	if len(out) > 0 {
		return WithPort53(out)
	}
	if dhcp, err := s.DHCPDNS(); err == nil && len(dhcp) > 0 {
		return WithPort53(dhcp)
	}
	return nil
}

// WithPort53 ensures every entry has an explicit :53 port.
func WithPort53(ips []string) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		if _, _, err := net.SplitHostPort(ip); err == nil {
			out[i] = ip
			continue
		}
		out[i] = net.JoinHostPort(ip, "53")
	}
	return out
}

// --- State/global DNS layer (SCDynamicStore via scutil) ------------------
//
// networksetup writes the *Setup* (persistent preferences) layer. When a
// full-tunnel VPN (Cisco AnyConnect / Secure Client with tunnel-all-dns) is
// connected it makes its utun the primary service and configd publishes
// State:/Network/Global/DNS pointing at the VPN resolver — and that global
// primary outranks anything in the per-service Setup layer. So to win
// resolver #1 while the tunnel is up we ALSO assert 127.0.0.1 at the State/
// global layer here.
//
// We shell out to `scutil` with a here-doc (open / d.init / d.add / set)
// routed through Runner rather than linking SystemConfiguration via cgo, so
// unit tests stay root-free and the daemon keeps a single Runner seam for
// every system mutation. AnyConnect re-clobbers global DNS on (re)connect,
// so the daemon's network watcher re-asserts this on its tick.
const globalDNSKey = "State:/Network/Global/DNS"

// globalDNSSetScript builds the scutil here-doc that overrides the global
// primary resolver list. servers are bare IPs (port stripped — the State
// layer carries addresses, not host:port).
func globalDNSSetScript(servers []string) string {
	var b strings.Builder
	b.WriteString("open\n")
	b.WriteString("d.init\n")
	b.WriteString("d.add ServerAddresses *")
	for _, ip := range servers {
		b.WriteString(" ")
		b.WriteString(stripPort(ip))
	}
	b.WriteString("\n")
	b.WriteString("set " + globalDNSKey + "\n")
	b.WriteString("quit\n")
	return "/usr/sbin/scutil <<'EOF'\n" + b.String() + "EOF\n"
}

func globalDNSRemoveScript() string {
	return "/usr/sbin/scutil <<'EOF'\nopen\nremove " + globalDNSKey + "\nquit\nEOF\n"
}

func globalDNSShowScript() string {
	return "/usr/sbin/scutil <<'EOF'\nshow " + globalDNSKey + "\nquit\nEOF\n"
}

// SetGlobalDNS asserts servers as the global primary resolver list at the
// State layer. Empty servers is treated as RemoveGlobalDNS so we never
// publish an empty (DNS-dead) global override.
func (s *SystemDNS) SetGlobalDNS(servers []string) error {
	if len(servers) == 0 {
		return s.RemoveGlobalDNS()
	}
	out, err := s.r.Run("sh", "-c", globalDNSSetScript(servers))
	if err != nil {
		return fmt.Errorf("scutil set global DNS: %w (%s)", err, string(out))
	}
	return nil
}

// RemoveGlobalDNS deletes our State-layer override so configd recomputes the
// global primary from the live network state (the per-service Setup layer or
// the VPN's published resolver). Idempotent: removing a key that isn't there
// is not an error we care about.
func (s *SystemDNS) RemoveGlobalDNS() error {
	out, err := s.r.Run("sh", "-c", globalDNSRemoveScript())
	if err != nil {
		return fmt.Errorf("scutil remove global DNS: %w (%s)", err, string(out))
	}
	return nil
}

// GlobalDNS returns the current State:/Network/Global/DNS ServerAddresses, in
// order (resolver #1 first). Returns nil when the key is absent.
func (s *SystemDNS) GlobalDNS() ([]string, error) {
	out, err := s.r.Run("sh", "-c", globalDNSShowScript())
	if err != nil {
		return nil, fmt.Errorf("scutil show global DNS: %w (%s)", err, string(out))
	}
	return parseGlobalDNS(string(out)), nil
}

// parseGlobalDNS extracts the ordered ServerAddresses array from the output
// of `scutil` `show State:/Network/Global/DNS`. "No such key" → nil.
func parseGlobalDNS(out string) []string {
	if strings.Contains(out, "No such key") {
		return nil
	}
	entryRe := regexp.MustCompile(`^\s*\d+\s*:\s*(\S+)`)
	var servers []string
	inArray := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ServerAddresses") && strings.Contains(trimmed, "<array>") {
			inArray = true
			continue
		}
		if !inArray {
			continue
		}
		if strings.HasPrefix(trimmed, "}") {
			break
		}
		if m := entryRe.FindStringSubmatch(line); m != nil {
			servers = append(servers, m[1])
		}
	}
	return servers
}

// GlobalPrimaryIsLoopback reports whether the global primary resolver (#1) is
// 127.0.0.1 — i.e. em-wall is currently winning resolver priority. Used by
// the watcher to decide whether to re-assert after the VPN clobbers it.
func (s *SystemDNS) GlobalPrimaryIsLoopback() (bool, error) {
	servers, err := s.GlobalDNS()
	if err != nil {
		return false, err
	}
	return len(servers) > 0 && isLoopback(stripPort(servers[0])), nil
}

// resolverView summarizes `scutil --dns` from em-wall's perspective: who
// actually answers a generic name, and which domains the VPN has scoped away
// from us. Setting State:/Network/Global/DNS only controls the *default*
// resolver; a full-tunnel VPN like AnyConnect additionally installs
// Supplemental resolvers scoped to specific domains (e.g. corp.example.com)
// that win for those names regardless of the global primary — so those names
// bypass em-wall entirely. This view exposes that gap honestly instead of the
// State-key check claiming a clean win.
type resolverView struct {
	DefaultServers    []string // nameservers of the catch-all default resolver, highest priority first
	DefaultIsLoopback bool     // the default resolver is 127.0.0.1 → em-wall is in the path for generic names
	VPNScopedDomains  []string // every domain a non-loopback tunnel resolver is scoped to (the VPN's split-DNS set)
	BypassDomains     []string // VPN-scoped domains a loopback resolver does NOT yet out-order → still skip em-wall
	ShadowedDomains   []string // VPN-scoped domains our loopback shadow out-orders → now captured
	MinVPNOrder       int      // lowest scutil `order` among VPN-scoped resolvers (0 if none) — our shadow must beat it
}

// ResolverView parses `scutil --dns` into a resolverView. See parseResolverView.
func (s *SystemDNS) ResolverView() (resolverView, error) {
	out, err := s.r.Run("scutil", "--dns")
	if err != nil {
		return resolverView{}, fmt.Errorf("scutil --dns: %w (%s)", err, string(out))
	}
	return parseResolverView(string(out)), nil
}

// parseResolverView interprets the main "DNS configuration" section of
// `scutil --dns` (resolvers are listed highest-priority first; the
// "(for scoped queries)" section is per-interface scoping we ignore here).
//
//   - The DEFAULT resolver is the first block with no domain restriction and
//     not mDNS — that's what answers an arbitrary public name. If its first
//     nameserver is loopback, em-wall is winning the default path.
//   - BYPASS domains are domain-scoped blocks bound to a tunnel interface
//     whose resolver isn't us — those names (the VPN's split-DNS) reach the
//     VPN directly, never passing through em-wall's rule engine.
func parseResolverView(out string) resolverView {
	if i := strings.Index(out, "DNS configuration (for scoped queries)"); i >= 0 {
		out = out[:i]
	}
	nsRe := regexp.MustCompile(`nameserver\[\d+\]\s*:\s*(\S+)`)
	domRe := regexp.MustCompile(`domain(?:\[\d+\])?\s*:\s*(\S+)`)
	ifRe := regexp.MustCompile(`if_index\s*:\s*\d+\s*\((\S+)\)`)
	orderRe := regexp.MustCompile(`order\s*:\s*(\d+)`)

	const noOrder = int(^uint(0) >> 1) // max int — "no order line" sorts last

	type block struct {
		servers []string
		domains []string
		iface   string
		mdns    bool
		order   int
	}
	var blocks []block
	var cur *block
	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "resolver #"):
			flush()
			cur = &block{order: noOrder}
		case cur == nil:
			// before the first resolver block — skip
		case strings.HasPrefix(t, "nameserver["):
			if m := nsRe.FindStringSubmatch(line); m != nil {
				cur.servers = append(cur.servers, m[1])
			}
		case strings.HasPrefix(t, "domain") || strings.HasPrefix(t, "search domain"):
			if m := domRe.FindStringSubmatch(line); m != nil {
				cur.domains = append(cur.domains, m[1])
			}
		case strings.HasPrefix(t, "if_index"):
			if m := ifRe.FindStringSubmatch(line); m != nil {
				cur.iface = m[1]
			}
		case strings.HasPrefix(t, "order"):
			if m := orderRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					cur.order = n
				}
			}
		case strings.HasPrefix(t, "options") && strings.Contains(t, "mdns"):
			cur.mdns = true
		}
	}
	flush()

	var view resolverView
	// Default resolver: first non-domain, non-mDNS block (listing order == priority).
	for _, b := range blocks {
		if b.mdns || len(b.domains) != 0 || len(b.servers) == 0 {
			continue
		}
		view.DefaultServers = b.servers
		view.DefaultIsLoopback = isLoopback(stripPort(b.servers[0]))
		break
	}
	// Per domain, track the best (lowest) order of a loopback resolver scoped
	// to it (our shadow) vs a VPN tunnel resolver. Whoever has the lower order
	// actually answers that domain — that's what decides captured vs bypass.
	type dom struct{ vpnOrder, loopOrder int }
	doms := map[string]*dom{}
	var vpnSeen []string
	seen := map[string]bool{}
	get := func(d string) *dom {
		if doms[d] == nil {
			doms[d] = &dom{vpnOrder: noOrder, loopOrder: noOrder}
		}
		return doms[d]
	}
	for _, b := range blocks {
		if b.mdns || len(b.domains) == 0 {
			continue
		}
		lb := len(b.servers) > 0 && isLoopback(stripPort(b.servers[0]))
		for _, d := range b.domains {
			di := get(d)
			if lb {
				if b.order < di.loopOrder {
					di.loopOrder = b.order
				}
			} else if isTunnelIface(b.iface) {
				if b.order < di.vpnOrder {
					di.vpnOrder = b.order
				}
				if !seen[d] {
					seen[d] = true
					vpnSeen = append(vpnSeen, d)
				}
			}
		}
	}
	view.MinVPNOrder = noOrder
	for _, d := range vpnSeen {
		di := doms[d]
		view.VPNScopedDomains = append(view.VPNScopedDomains, d)
		// Captured only when our loopback shadow actually out-orders the VPN.
		if di.loopOrder < di.vpnOrder {
			view.ShadowedDomains = append(view.ShadowedDomains, d)
		} else {
			view.BypassDomains = append(view.BypassDomains, d)
		}
		if di.vpnOrder < view.MinVPNOrder {
			view.MinVPNOrder = di.vpnOrder
		}
	}
	if view.MinVPNOrder == noOrder {
		view.MinVPNOrder = 0 // no VPN-scoped resolver seen
	}
	return view
}

// --- Split-DNS shadowing (per-domain /etc/resolver files) ------------------
//
// A full-tunnel VPN installs Supplemental resolvers scoped to its internal
// domains (e.g. corp.example), and macOS routes those names to the resolver
// with the lowest `order` for the longest matching domain — so they bypass
// em-wall even when 127.0.0.1 is the global primary. To capture them we write
// a per-domain /etc/resolver/<domain> file pointing at 127.0.0.1 with a
// `search_order` LOWER than the VPN's, so we win the tie for those exact
// names. em-wall then sees the query, applies rules, and forwards "allow" to
// the VPN's resolver (chooseUpstream ranks the tunnel resolver first), so
// internal names still resolve.
//
// Why /etc/resolver and not SCDynamicStore: scutil writes SupplementalMatch
// Orders as CFStrings, which configd ignores — it then assigns our shadow a
// HIGHER (losing) order. /etc/resolver's `search_order` is parsed by libresolv
// as a real number and honored, so we can deterministically out-order the VPN.
//
// resolverFileMarker tags files we created so we only ever remove/overwrite
// our own — never a user's hand-written /etc/resolver entry.
const resolverFileMarker = "# em-wall: split-DNS shadow (managed; safe to delete)"

// defaultSupplementalOrder is used when the VPN's order can't be determined.
// Lower beats higher; only compared against resolvers for the SAME domain.
const defaultSupplementalOrder = 100

// validDNSLabelDomain reports whether s is a safe domain to use as a resolver
// filename — letters/digits/dot/hyphen only. Domains come from parsing scutil
// output, but we validate defensively against path traversal / junk.
var validDNSLabelDomain = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// SetSupplementalResolver makes em-wall the resolver for each domain by writing
// /etc/resolver/<domain> → 127.0.0.1 with search_order `order` (so it must be
// below the VPN's order to win). Stale shadow files for domains no longer in
// the set are removed. A pre-existing file we didn't create is left untouched.
// An empty/all-invalid set tears every shadow down.
func (s *SystemDNS) SetSupplementalResolver(domains []string, order int) error {
	if order < 1 {
		order = defaultSupplementalOrder
	}
	want := map[string]bool{}
	var clean []string
	for _, d := range domains {
		d = strings.TrimSuffix(strings.TrimSpace(d), ".")
		if d == "" || want[d] || !validDNSLabelDomain.MatchString(d) {
			continue
		}
		want[d] = true
		clean = append(clean, d)
	}
	// Remove our stale shadow files (ours, but no longer wanted).
	if err := s.pruneResolverFiles(want); err != nil {
		return err
	}
	if len(clean) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.resolverDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.resolverDir, err)
	}
	content := fmt.Sprintf("%s\nnameserver 127.0.0.1\nsearch_order %d\n", resolverFileMarker, order)
	var firstErr error
	for _, d := range clean {
		path := filepath.Join(s.resolverDir, d)
		// Never clobber a file we didn't create.
		if existing, err := os.ReadFile(path); err == nil && !isOurResolverFile(existing) {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("write %s: %w", path, err)
		}
	}
	return firstErr
}

// RemoveSupplementalResolver deletes every shadow file em-wall created.
// Idempotent and leaves user-authored /etc/resolver files alone.
func (s *SystemDNS) RemoveSupplementalResolver() error {
	return s.pruneResolverFiles(nil)
}

// pruneResolverFiles removes each em-wall-managed file in resolverDir whose
// domain is not in keep. A nil/empty keep removes all of ours.
func (s *SystemDNS) pruneResolverFiles(keep map[string]bool) error {
	entries, err := os.ReadDir(s.resolverDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.resolverDir, err)
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		path := filepath.Join(s.resolverDir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil || !isOurResolverFile(body) {
			continue // unreadable or not ours — leave it
		}
		if err := os.Remove(path); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return firstErr
}

func isOurResolverFile(body []byte) bool {
	return strings.HasPrefix(string(body), resolverFileMarker)
}

// TunnelResolvers returns the resolvers bound to a tunnel interface
// (utun/ipsec/…) in `scutil --dns`. This is the VPN's own resolver — the one
// that can answer internal/corporate names — isolated from non-tunnel DHCP
// DNS (AllDHCPDNS) and public resolvers, so the daemon can make it the
// deterministic fall-through upstream while AnyConnect is connected.
//
// exclude lists interface names to ignore — em-wall opens its OWN utun for
// proxy routing, and although that tunnel pushes no DNS, excluding it makes
// sure the daemon can never mistake its own interface for a VPN's resolver.
func (s *SystemDNS) TunnelResolvers(exclude ...string) ([]string, error) {
	out, err := s.r.Run("scutil", "--dns")
	if err != nil {
		return nil, fmt.Errorf("scutil --dns: %w (%s)", err, string(out))
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if e != "" {
			skip[e] = true
		}
	}
	return parseTunnelResolvers(string(out), skip), nil
}

// parseTunnelResolvers walks the resolver blocks of `scutil --dns` output and
// collects, de-duplicated and in order, every nameserver that sits in a block
// whose if_index names a tunnel interface that isn't in skip. Loopback
// entries are skipped.
func parseTunnelResolvers(out string, skip map[string]bool) []string {
	nsRe := regexp.MustCompile(`nameserver\[\d+\]\s*:\s*(\S+)`)
	ifRe := regexp.MustCompile(`if_index\s*:\s*\d+\s*\((\S+)\)`)

	var blockServers []string
	blockIface := ""
	seen := map[string]bool{}
	var out2 []string

	flush := func() {
		if isTunnelIface(blockIface) && !skip[blockIface] {
			for _, ip := range blockServers {
				if isLoopback(ip) || seen[ip] {
					continue
				}
				seen[ip] = true
				out2 = append(out2, ip)
			}
		}
		blockServers = nil
		blockIface = ""
	}

	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "resolver #") {
			flush()
			continue
		}
		if m := nsRe.FindStringSubmatch(line); m != nil {
			blockServers = append(blockServers, m[1])
			continue
		}
		if m := ifRe.FindStringSubmatch(line); m != nil {
			blockIface = m[1]
		}
	}
	flush()
	return out2
}

func isLoopback(ip string) bool {
	if ip == "::1" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed != nil {
		return parsed.IsLoopback()
	}
	return strings.HasPrefix(ip, "127.")
}
