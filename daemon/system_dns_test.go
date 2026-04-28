package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
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
