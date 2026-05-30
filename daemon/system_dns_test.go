package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

type stubRunner struct {
	mu    sync.Mutex
	calls [][]string
	// out maps a key (joined argv) to a stub response.
	out map[string][]byte
	err map[string]error
}

func newStubRunner() *stubRunner {
	return &stubRunner{
		out: map[string][]byte{},
		err: map[string]error{},
	}
}

func (r *stubRunner) on(argv []string, body string, err error) {
	key := strings.Join(argv, " ")
	r.out[key] = []byte(body)
	if err != nil {
		r.err[key] = err
	}
}

func (r *stubRunner) Run(name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	full := append([]string{name}, args...)
	key := strings.Join(full, " ")
	r.calls = append(r.calls, full)
	if e, ok := r.err[key]; ok {
		return r.out[key], e
	}
	if b, ok := r.out[key]; ok {
		return b, nil
	}
	return nil, errors.New("stub: no canned response for: " + key)
}

func TestListServices(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-listallnetworkservices"},
		"An asterisk (*) denotes that a network service is disabled.\nWi-Fi\nThunderbolt Bridge\n*Bluetooth PAN\nEthernet\n", nil)
	s := NewSystemDNS(r)
	got, err := s.ListServices()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Wi-Fi", "Thunderbolt Bridge", "Ethernet"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGetServiceDNS_Manual(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-getdnsservers", "Wi-Fi"}, "1.1.1.1\n8.8.8.8\n", nil)
	s := NewSystemDNS(r)
	got, err := s.GetServiceDNS("Wi-Fi")
	if err != nil {
		t.Fatal(err)
	}
	if !equalSlice(got, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Errorf("got %v", got)
	}
}

func TestGetServiceDNS_DHCP(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-getdnsservers", "Wi-Fi"},
		"There aren't any DNS Servers set on Wi-Fi.\n", nil)
	s := NewSystemDNS(r)
	got, err := s.GetServiceDNS("Wi-Fi")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for DHCP, got %v", got)
	}
}

func TestSetServiceDNS_Empty(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-setdnsservers", "Wi-Fi", "Empty"}, "", nil)
	s := NewSystemDNS(r)
	if err := s.SetServiceDNS("Wi-Fi", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSetServiceDNS_Multiple(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-setdnsservers", "Wi-Fi", "127.0.0.1"}, "", nil)
	s := NewSystemDNS(r)
	if err := s.SetServiceDNS("Wi-Fi", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
}

func TestDetectResolvers_SkipsLoopback(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"scutil", "--dns"}, `
DNS configuration

resolver #1
  search domain[0] : home
  nameserver[0] : 127.0.0.1
  nameserver[1] : 192.168.1.1
  nameserver[2] : 8.8.8.8
  if_index : 16 (en0)
  flags    : Request A records, Request AAAA records
`, nil)
	s := NewSystemDNS(r)
	got, err := s.DetectResolvers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.1.1", "8.8.8.8"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPrimaryInterface(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"route", "-n", "get", "default"}, `
   route to: default
destination: default
       mask: default
    gateway: 192.168.1.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>
`, nil)
	s := NewSystemDNS(r)
	got, err := s.PrimaryInterface()
	if err != nil {
		t.Fatal(err)
	}
	if got != "en0" {
		t.Errorf("got %q", got)
	}
}

func TestCaptureAndRestore(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-listallnetworkservices"},
		"Header line\nWi-Fi\nEthernet\n", nil)
	r.on([]string{"networksetup", "-getdnsservers", "Wi-Fi"}, "1.1.1.1\n", nil)
	r.on([]string{"networksetup", "-getdnsservers", "Ethernet"},
		"There aren't any DNS Servers set on Ethernet.\n", nil)
	r.on([]string{"networksetup", "-setdnsservers", "Wi-Fi", "1.1.1.1"}, "", nil)
	r.on([]string{"networksetup", "-setdnsservers", "Ethernet", "Empty"}, "", nil)
	s := NewSystemDNS(r)
	snap, err := s.CaptureAll()
	if err != nil {
		t.Fatal(err)
	}
	if !equalSlice(snap["Wi-Fi"], []string{"1.1.1.1"}) {
		t.Errorf("Wi-Fi capture = %v", snap["Wi-Fi"])
	}
	if snap["Ethernet"] != nil && len(snap["Ethernet"]) != 0 {
		t.Errorf("Ethernet should capture as nil/empty, got %v", snap["Ethernet"])
	}
	if err := s.RestoreAll(snap); err != nil {
		t.Fatal(err)
	}
}

func TestPickUpstream_FromSnapshot(t *testing.T) {
	r := newStubRunner()
	s := NewSystemDNS(r)
	snap := map[string][]string{
		"Wi-Fi":    {"192.168.1.1", "127.0.0.1"},
		"Ethernet": {"192.168.1.1", "8.8.8.8"},
	}
	got := s.PickUpstream(snap)
	// dedup, exclude loopback, port-suffixed
	for _, want := range []string{"192.168.1.1:53", "8.8.8.8:53"} {
		if !contains(got, want) {
			t.Errorf("expected %q in %v", want, got)
		}
	}
	for _, bad := range []string{"127.0.0.1:53", "127.0.0.1"} {
		if contains(got, bad) {
			t.Errorf("did not expect %q in %v", bad, got)
		}
	}
}

func TestWithPort53(t *testing.T) {
	got := WithPort53([]string{"1.1.1.1", "1.1.1.1:8053", "::1", "[::1]:53"})
	want := []string{"1.1.1.1:53", "1.1.1.1:8053", "[::1]:53", "[::1]:53"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAllDHCPDNS_SkipsTunnels(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-listallhardwareports"}, `
Hardware Port: Wi-Fi
Device: en0
Ethernet Address: aa:bb:cc:dd:ee:ff

Hardware Port: VPN tunnel
Device: utun5
Ethernet Address:

Hardware Port: Ethernet
Device: en1
Ethernet Address: 11:22:33:44:55:66
`, nil)
	r.on([]string{"ipconfig", "getoption", "en0", "domain_name_server"}, "192.168.1.1\n", nil)
	r.on([]string{"ipconfig", "getoption", "en1", "domain_name_server"}, "10.0.0.1\n", nil)
	// utun5 should NOT be queried.
	s := NewSystemDNS(r)
	got, err := s.AllDHCPDNS()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.1.1", "10.0.0.1"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, c := range r.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "utun5") {
			t.Errorf("should not have queried tunnel: %s", joined)
		}
	}
}

func TestAllDHCPDNS_DedupesAndSkipsLoopback(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-listallhardwareports"}, `
Hardware Port: Wi-Fi
Device: en0
Hardware Port: Eth
Device: en1
Hardware Port: Bridge
Device: bridge0
`, nil)
	r.on([]string{"ipconfig", "getoption", "en0", "domain_name_server"}, "192.168.1.1\n", nil)
	r.on([]string{"ipconfig", "getoption", "en1", "domain_name_server"}, "192.168.1.1\n", nil)
	r.on([]string{"ipconfig", "getoption", "bridge0", "domain_name_server"}, "127.0.0.1\n", nil)
	s := NewSystemDNS(r)
	got, err := s.AllDHCPDNS()
	if err != nil {
		t.Fatal(err)
	}
	if !equalSlice(got, []string{"192.168.1.1"}) {
		t.Errorf("got %v, want exactly [192.168.1.1]", got)
	}
}

func TestIsTunnelIface(t *testing.T) {
	cases := map[string]bool{
		"en0":    false,
		"en12":   false,
		"utun0":  true,
		"utun":   true,
		"ipsec0": true,
		"ppp0":   true,
		"tun3":   true,
		"tap1":   true,
		"":       false,
	}
	for name, want := range cases {
		if got := isTunnelIface(name); got != want {
			t.Errorf("isTunnelIface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsActive(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"networksetup", "-listallnetworkservices"},
		"Header\nWi-Fi\n", nil)
	r.on([]string{"networksetup", "-getdnsservers", "Wi-Fi"}, "127.0.0.1\n", nil)
	s := NewSystemDNS(r)
	active, err := s.IsActive()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Errorf("expected active")
	}
}

func TestParseTunnelResolvers(t *testing.T) {
	// Two scoped resolvers: one on a real VPN utun (Cisco AnyConnect),
	// one on Wi-Fi (en0). Only the utun one should come back.
	out := `
DNS configuration

resolver #1
  search domain[0] : home
  nameserver[0] : 192.168.1.1
  if_index : 16 (en0)
  flags    : Request A records

resolver #2
  domain   : corp.example.com
  nameserver[0] : 10.20.0.53
  nameserver[1] : 10.20.0.54
  if_index : 21 (utun4)
  flags    : Scoped, Request A records

DNS configuration (for scoped queries)

resolver #1
  nameserver[0] : 10.20.0.53
  if_index : 21 (utun4)
`
	got := parseTunnelResolvers(out, nil)
	want := []string{"10.20.0.53", "10.20.0.54"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseTunnelResolvers_ExcludesOwnUtun(t *testing.T) {
	// em-wall's own proxy utun (utun7) carries no DNS in practice, but if
	// it ever did we must never adopt it as the VPN resolver.
	out := `
resolver #1
  nameserver[0] : 10.20.0.53
  if_index : 21 (utun4)

resolver #2
  nameserver[0] : 100.64.0.1
  if_index : 30 (utun7)
`
	got := parseTunnelResolvers(out, map[string]bool{"utun7": true})
	want := []string{"10.20.0.53"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseTunnelResolvers_NoTunnel(t *testing.T) {
	out := `
resolver #1
  nameserver[0] : 192.168.1.1
  if_index : 16 (en0)
`
	if got := parseTunnelResolvers(out, nil); len(got) != 0 {
		t.Errorf("expected no tunnel resolvers, got %v", got)
	}
}

func TestTunnelResolvers_PassesExclude(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"scutil", "--dns"}, `
resolver #1
  nameserver[0] : 10.20.0.53
  if_index : 21 (utun4)
resolver #2
  nameserver[0] : 100.64.0.1
  if_index : 30 (utun7)
`, nil)
	s := NewSystemDNS(r)
	got, err := s.TunnelResolvers("utun7")
	if err != nil {
		t.Fatal(err)
	}
	if !equalSlice(got, []string{"10.20.0.53"}) {
		t.Errorf("got %v", got)
	}
}

// Shape of `scutil --dns` from a Mac with a full-tunnel VPN connected in
// split-DNS mode (fake corp domain / RFC-5737 resolver IPs): corp.example is
// Supplemental-scoped to the VPN's utun6, while 127.0.0.1 (em-wall) is the
// catch-all default resolver.
const anyconnectSplitDNS = `DNS configuration

resolver #1
  search domain[0] : corp.example
  nameserver[0] : 203.0.113.1
  nameserver[1] : 203.0.113.2
  if_index : 28 (utun6)
  flags    : Supplemental, Request A records
  reach    : 0x00000002 (Reachable)
  order    : 101201

resolver #2
  nameserver[0] : 127.0.0.1
  flags    : Request A records, Request AAAA records
  reach    : 0x00030002 (Reachable,Local Address,Directly Reachable Address)
  order    : 200000

resolver #3
  domain   : corp.example
  nameserver[0] : 203.0.113.1
  nameserver[1] : 203.0.113.2
  if_index : 28 (utun6)
  flags    : Supplemental, Request A records
  reach    : 0x00000002 (Reachable)
  order    : 101200

resolver #4
  domain   : local
  options  : mdns
  timeout  : 5
  flags    : Request A records
  reach    : 0x00000000 (Not Reachable)
  order    : 300000

DNS configuration (for scoped queries)

resolver #1
  nameserver[0] : 127.0.0.1
  if_index : 14 (en0)
  flags    : Scoped, Request A records, Request AAAA records
  reach    : 0x00000000 (Not Reachable)

resolver #2
  search domain[0] : corp.example
  nameserver[0] : 203.0.113.1
  nameserver[1] : 203.0.113.2
  if_index : 28 (utun6)
  flags    : Scoped, Request A records
  reach    : 0x00000002 (Reachable)
`

func TestParseResolverView_AnyConnectSplitDNS(t *testing.T) {
	v := parseResolverView(anyconnectSplitDNS)
	// em-wall (127.0.0.1) is the catch-all default → in the path for public names.
	if !v.DefaultIsLoopback {
		t.Errorf("expected default resolver to be loopback, got %v", v.DefaultServers)
	}
	// corp.example is scoped to the VPN and bypasses us (deduped across #1/#3).
	if !equalSlice(v.VPNScopedDomains, []string{"corp.example"}) {
		t.Errorf("VPNScopedDomains = %v, want [corp.example]", v.VPNScopedDomains)
	}
	if !equalSlice(v.BypassDomains, []string{"corp.example"}) {
		t.Errorf("BypassDomains = %v, want [corp.example]", v.BypassDomains)
	}
	if len(v.ShadowedDomains) != 0 {
		t.Errorf("ShadowedDomains = %v, want none", v.ShadowedDomains)
	}
}

// MinVPNOrder is the lowest VPN order among the scoped domains — what our
// shadow has to beat. From the AnyConnect split-DNS fixture that's 101200.
func TestParseResolverView_MinVPNOrder(t *testing.T) {
	v := parseResolverView(anyconnectSplitDNS)
	if v.MinVPNOrder != 101200 {
		t.Errorf("MinVPNOrder = %d, want 101200", v.MinVPNOrder)
	}
}

// A loopback shadow that OUT-orders the VPN (lower order) captures the domain.
func TestParseResolverView_ShadowWins(t *testing.T) {
	out := `DNS configuration

resolver #1
  domain   : corp.example
  nameserver[0] : 127.0.0.1
  flags    : Supplemental, Request A records
  order    : 101199

resolver #2
  domain   : corp.example
  nameserver[0] : 203.0.113.1
  if_index : 28 (utun6)
  flags    : Supplemental, Request A records
  order    : 101200

resolver #3
  nameserver[0] : 127.0.0.1
  flags    : Request A records
  order    : 200000
`
	v := parseResolverView(out)
	if !equalSlice(v.ShadowedDomains, []string{"corp.example"}) {
		t.Errorf("ShadowedDomains = %v, want [corp.example]", v.ShadowedDomains)
	}
	if len(v.BypassDomains) != 0 {
		t.Errorf("BypassDomains = %v, want none (captured)", v.BypassDomains)
	}
}

// A loopback shadow that LOSES the order (higher order than the VPN) does NOT
// capture the domain — this is the real failure the user hit (our 103000 vs
// the VPN's 101200), so the status must still report it as bypassing.
func TestParseResolverView_ShadowLoses(t *testing.T) {
	out := `DNS configuration

resolver #1
  domain   : corp.example
  nameserver[0] : 203.0.113.1
  if_index : 28 (utun6)
  flags    : Supplemental, Request A records
  order    : 101200

resolver #2
  domain   : corp.example
  nameserver[0] : 127.0.0.1
  flags    : Supplemental, Request A records, Request AAAA records
  order    : 103000

resolver #3
  nameserver[0] : 127.0.0.1
  flags    : Request A records
  order    : 200000
`
	v := parseResolverView(out)
	if !equalSlice(v.BypassDomains, []string{"corp.example"}) {
		t.Errorf("BypassDomains = %v, want [corp.example] (shadow loses the order)", v.BypassDomains)
	}
	if len(v.ShadowedDomains) != 0 {
		t.Errorf("ShadowedDomains = %v, want none (we lost)", v.ShadowedDomains)
	}
}

func TestSetSupplementalResolver_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewSystemDNS(newStubRunner())
	s.resolverDir = dir
	if err := s.SetSupplementalResolver([]string{"corp.example", "internal.example"}, 101199); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"corp.example", "internal.example"} {
		body, err := os.ReadFile(filepath.Join(dir, d))
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		text := string(body)
		if !strings.Contains(text, "nameserver 127.0.0.1") || !strings.Contains(text, "search_order 101199") {
			t.Errorf("%s content:\n%s", d, text)
		}
		if !isOurResolverFile(body) {
			t.Errorf("%s missing marker", d)
		}
	}
}

// Our shadows are pruned when the domain set changes / is cleared, but a
// user-authored /etc/resolver file (no marker) is never touched.
func TestSetSupplementalResolver_PrunesOursPreservesUsers(t *testing.T) {
	dir := t.TempDir()
	s := NewSystemDNS(newStubRunner())
	s.resolverDir = dir

	userFile := filepath.Join(dir, "mydomain.test")
	if err := os.WriteFile(userFile, []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.SetSupplementalResolver([]string{"corp.example"}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "corp.example")); err != nil {
		t.Fatalf("corp.example should exist: %v", err)
	}
	// Switch the set — corp.example (ours) is pruned, other.example written.
	if err := s.SetSupplementalResolver([]string{"other.example"}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "corp.example")); !os.IsNotExist(err) {
		t.Errorf("corp.example should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "other.example")); err != nil {
		t.Errorf("other.example should exist: %v", err)
	}
	// Remove-all drops ours but leaves the user file intact.
	if err := s.RemoveSupplementalResolver(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.example")); !os.IsNotExist(err) {
		t.Errorf("other.example should have been removed")
	}
	if b, err := os.ReadFile(userFile); err != nil || string(b) != "nameserver 10.0.0.1\n" {
		t.Errorf("user resolver file must survive untouched, got %q err %v", string(b), err)
	}
}

func TestSetSupplementalResolver_InvalidDropped(t *testing.T) {
	dir := t.TempDir()
	s := NewSystemDNS(newStubRunner())
	s.resolverDir = dir
	// "bad/../etc" has a slash → rejected (no path traversal); ok.example kept.
	if err := s.SetSupplementalResolver([]string{"bad/../etc", "ok.example"}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ok.example")); err != nil {
		t.Errorf("ok.example should exist: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected only ok.example, got %d entries", len(entries))
	}
}

func TestParseResolverView_TunnelAllDNS(t *testing.T) {
	// Full tunnel-all-dns: the VPN's resolver is the catch-all default (no
	// domain restriction) bound to utun, so em-wall is NOT in the path.
	out := `DNS configuration

resolver #1
  nameserver[0] : 10.0.0.53
  if_index : 21 (utun4)
  flags    : Request A records
  order    : 100000

resolver #2
  nameserver[0] : 127.0.0.1
  flags    : Request A records
  order    : 200000
`
	v := parseResolverView(out)
	if v.DefaultIsLoopback {
		t.Errorf("expected default NOT loopback (VPN owns default), got loopback")
	}
	if len(v.BypassDomains) != 0 {
		t.Errorf("no domain-scoped bypass expected, got %v", v.BypassDomains)
	}
}

func TestParseResolverView_NoVPN(t *testing.T) {
	out := `DNS configuration

resolver #1
  nameserver[0] : 127.0.0.1
  flags    : Request A records
  order    : 200000
`
	v := parseResolverView(out)
	if !v.DefaultIsLoopback || len(v.BypassDomains) != 0 {
		t.Errorf("got loopback=%v bypass=%v", v.DefaultIsLoopback, v.BypassDomains)
	}
}

func TestParseGlobalDNS(t *testing.T) {
	out := `<dictionary> {
  ServerAddresses : <array> {
    0 : 127.0.0.1
    1 : 8.8.8.8
  }
  DomainName : example.com
}`
	got := parseGlobalDNS(out)
	want := []string{"127.0.0.1", "8.8.8.8"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseGlobalDNS_NoSuchKey(t *testing.T) {
	if got := parseGlobalDNS("  No such key\n"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSetGlobalDNS_RunsScutil(t *testing.T) {
	r := newStubRunner()
	script := globalDNSSetScript([]string{"127.0.0.1"})
	r.on([]string{"sh", "-c", script}, "", nil)
	s := NewSystemDNS(r)
	if err := s.SetGlobalDNS([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	// The script must target the global key and carry the loopback IP
	// without a :53 port (State layer holds addresses, not host:port).
	if !strings.Contains(script, "set "+globalDNSKey) ||
		!strings.Contains(script, "ServerAddresses * 127.0.0.1\n") {
		t.Errorf("unexpected script:\n%s", script)
	}
}

func TestSetGlobalDNS_EmptyRemoves(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"sh", "-c", globalDNSRemoveScript()}, "", nil)
	s := NewSystemDNS(r)
	if err := s.SetGlobalDNS(nil); err != nil {
		t.Fatal(err)
	}
	// Must have invoked the remove script, not a set with an empty array.
	joined := strings.Join(r.calls[len(r.calls)-1], " ")
	if !strings.Contains(joined, "remove "+globalDNSKey) {
		t.Errorf("expected remove, got %s", joined)
	}
}

func TestGlobalPrimaryIsLoopback(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"sh", "-c", globalDNSShowScript()}, `<dictionary> {
  ServerAddresses : <array> {
    0 : 127.0.0.1
  }
}`, nil)
	s := NewSystemDNS(r)
	win, err := s.GlobalPrimaryIsLoopback()
	if err != nil {
		t.Fatal(err)
	}
	if !win {
		t.Errorf("expected winning (127.0.0.1 first)")
	}
}

func TestGlobalPrimaryIsLoopback_VPNWins(t *testing.T) {
	r := newStubRunner()
	r.on([]string{"sh", "-c", globalDNSShowScript()}, `<dictionary> {
  ServerAddresses : <array> {
    0 : 10.20.0.53
    1 : 127.0.0.1
  }
}`, nil)
	s := NewSystemDNS(r)
	win, err := s.GlobalPrimaryIsLoopback()
	if err != nil {
		t.Fatal(err)
	}
	if win {
		t.Errorf("expected NOT winning when VPN resolver is #1")
	}
}

func newDaemonTestStore(t *testing.T) *rules.Store {
	t.Helper()
	s, err := rules.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// calledScript reports whether the stub saw an `sh -c <script>` invocation.
func calledScript(r *stubRunner, script string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if len(c) == 3 && c[0] == "sh" && c[1] == "-c" && c[2] == script {
			return true
		}
	}
	return false
}

// TestAssertGlobalDNSOverride locks in the daemon-level guarantee: the
// State-layer 127.0.0.1 override is written ONLY when the feature is opted
// into AND the per-service hijack is active. Either condition false → no
// write, so we never make ourselves resolver #1 while the hijack is off.
func TestAssertGlobalDNSOverride(t *testing.T) {
	ctx := context.Background()
	setScript := globalDNSSetScript([]string{"127.0.0.1"})

	newDeps := func(active bool, priority string) (*handlerDeps, *stubRunner) {
		r := newStubRunner()
		r.on([]string{"networksetup", "-listallnetworkservices"}, "Header\nWi-Fi\n", nil)
		dns := "192.168.1.1\n" // not loopback → IsActive false
		if active {
			dns = "127.0.0.1\n" // loopback → IsActive true
		}
		r.on([]string{"networksetup", "-getdnsservers", "Wi-Fi"}, dns, nil)
		r.on([]string{"sh", "-c", setScript}, "", nil)
		store := newDaemonTestStore(t)
		if priority != "" {
			if err := store.SetSetting(ctx, settingVPNDNSPriority, priority); err != nil {
				t.Fatal(err)
			}
		}
		return &handlerDeps{store: store, sysDNS: NewSystemDNS(r)}, r
	}

	t.Run("active and enabled writes override", func(t *testing.T) {
		d, r := newDeps(true, "true")
		set, err := d.assertGlobalDNSOverride(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !set || !calledScript(r, setScript) {
			t.Errorf("expected override to be written (set=%v)", set)
		}
	})

	t.Run("inactive does NOT write override", func(t *testing.T) {
		d, r := newDeps(false, "true")
		set, err := d.assertGlobalDNSOverride(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if set || calledScript(r, setScript) {
			t.Errorf("must not assert 127.0.0.1 as resolver while hijack inactive")
		}
	})

	t.Run("feature disabled does NOT write override", func(t *testing.T) {
		d, r := newDeps(true, "false")
		set, err := d.assertGlobalDNSOverride(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if set || calledScript(r, setScript) {
			t.Errorf("must not assert override when feature disabled")
		}
	})
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestParseDNSServers(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantBad string
	}{
		{"empty", "", nil, ""},
		{"whitespace only", "  \n\t ", nil, ""},
		{"single bare ipv4", "8.8.8.8", []string{"8.8.8.8:53"}, ""},
		{"explicit port kept", "8.8.8.8:5353", []string{"8.8.8.8:5353"}, ""},
		{"commas and newlines mixed", "1.1.1.1, 8.8.8.8\n9.9.9.9", []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}, ""},
		{"semicolons and spaces", "1.1.1.1 ; 8.8.4.4", []string{"1.1.1.1:53", "8.8.4.4:53"}, ""},
		{"dedup", "8.8.8.8\n8.8.8.8:53\n8.8.8.8", []string{"8.8.8.8:53"}, ""},
		{"bare ipv6 gets bracketed :53", "2001:4860:4860::8888", []string{"[2001:4860:4860::8888]:53"}, ""},
		{"ipv6 with port", "[2606:4700:4700::1111]:53", []string{"[2606:4700:4700::1111]:53"}, ""},
		{"invalid hostname rejected", "8.8.8.8, dns.google", nil, "dns.google"},
		{"garbage rejected", "not-an-ip", nil, "not-an-ip"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, bad := parseDNSServers(c.in)
			if bad != c.wantBad {
				t.Fatalf("invalid token = %q, want %q", bad, c.wantBad)
			}
			if !equalSlice(got, c.want) {
				t.Errorf("servers = %v, want %v", got, c.want)
			}
		})
	}
}
