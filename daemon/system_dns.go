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
	"os/exec"
	"regexp"
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
}

func NewSystemDNS(r Runner) *SystemDNS {
	if r == nil {
		r = execRunner{}
	}
	return &SystemDNS{r: r}
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
