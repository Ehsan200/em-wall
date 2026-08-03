package groups

import (
	"net"
	"strings"
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

const googFeed = `{
  "syncToken": "1",
  "creationTime": "2026-08-03T00:00:00",
  "prefixes": [
    {"ipv4Prefix": "142.250.0.0/15"},
    {"ipv4Prefix": "74.125.0.0/16"},
    {"ipv4Prefix": "8.8.8.0/24"},
    {"ipv4Prefix": "34.64.0.0/11"},
    {"ipv6Prefix": "2001:4860:4000::/36"},
    {"ipv4Prefix": "not-an-ip"},
    {"ipv4Prefix": "74.125.0.0/16"}
  ]
}`

const cloudFeed = `{
  "prefixes": [
    {"ipv4Prefix": "34.64.0.0/11"},
    {"ipv6Prefix": "2600:1900::/28"}
  ]
}`

func googleSource() *DynamicSource {
	return &DynamicSource{
		URL:        "https://example.invalid/goog.json",
		ExcludeURL: "https://example.invalid/cloud.json",
		Format:     FormatGoogleIPRanges,
		IPv4Only:   true,
		Exclude:    []string{"8.8.8.0/24"},
	}
}

func TestDynamicSource_ParseGoogleIPRanges(t *testing.T) {
	got, err := googleSource().Parse([]byte(googFeed), []byte(cloudFeed))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"142.250.0.0/15", "74.125.0.0/16"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	}
}

// The exclusion feed is what keeps GCP customer ranges — where unrelated
// third parties live — out of the group.
func TestDynamicSource_SubtractsCloudRanges(t *testing.T) {
	got, err := googleSource().Parse([]byte(googFeed), []byte(cloudFeed))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p == "34.64.0.0/11" {
			t.Fatalf("GCP customer range leaked into the group: %v", got)
		}
	}
}

// Google Public DNS must never be pinned to the proxy utun — the daemon may
// be forwarding upstream queries there.
func TestDynamicSource_ExcludesPublicDNS(t *testing.T) {
	got, err := googleSource().Parse([]byte(googFeed), []byte(cloudFeed))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "8.8.") {
			t.Fatalf("Google Public DNS leaked into the group: %v", got)
		}
	}
}

func TestDynamicSource_DropsIPv6WhenV4Only(t *testing.T) {
	got, err := googleSource().Parse([]byte(googFeed), []byte(cloudFeed))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		ip, _, err := net.ParseCIDR(p)
		if err != nil {
			t.Fatalf("unparseable prefix %q", p)
		}
		if ip.To4() == nil {
			t.Fatalf("IPv6 prefix %q emitted under IPv4Only", p)
		}
	}
}

// An empty parse result must be an error, not an empty list: the caller
// deletes rules for prefixes that vanish, so a reshaped feed would otherwise
// silently wipe the group.
func TestDynamicSource_EmptyResultIsError(t *testing.T) {
	src := googleSource()
	if _, err := src.Parse([]byte(`{"prefixes":[]}`), nil); err == nil {
		t.Fatal("expected error for empty feed")
	}
	if _, err := src.Parse([]byte(googFeed), []byte(googFeed)); err == nil {
		t.Fatal("expected error when the exclusion feed removes everything")
	}
}

func TestDynamicSource_RejectsGarbage(t *testing.T) {
	src := googleSource()
	if _, err := src.Parse([]byte("<html>blocked</html>"), nil); err == nil {
		t.Fatal("expected error for non-JSON feed")
	}
	bad := &DynamicSource{Format: "nope"}
	if _, err := bad.Parse([]byte(googFeed), nil); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestDynamicSource_CanonicalizesHostBits(t *testing.T) {
	got, err := (&DynamicSource{Format: FormatGoogleIPRanges, IPv4Only: true}).
		Parse([]byte(`{"prefixes":[{"ipv4Prefix":"74.125.4.5/16"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "74.125.0.0/16" {
		t.Fatalf("got %q, want canonical 74.125.0.0/16", got[0])
	}
}

func TestDynamicSource_DefaultInterval(t *testing.T) {
	var nilSrc *DynamicSource
	if nilSrc.EffectiveInterval() != DefaultDynamicInterval {
		t.Fatal("nil source must yield the default interval")
	}
	if (&DynamicSource{}).EffectiveInterval() != DefaultDynamicInterval {
		t.Fatal("unset interval must yield the default")
	}
}

// The google-media group exists to catch Meet/WebRTC media, which arrives as
// raw IPs out of the SDP — so every seed pattern must be a routable IP rule
// (domain patterns would be useless here), and the group must be dynamic.
func TestGoogleMedia_SeedIsIPv4Rules(t *testing.T) {
	g := FindByKey("google-media")
	if g == nil {
		t.Fatal("google-media group missing")
	}
	if g.Dynamic == nil || g.Dynamic.URL == "" {
		t.Fatal("google-media must carry a dynamic source")
	}
	for _, p := range g.Patterns {
		if !rules.IsIPRule(p) {
			t.Errorf("seed pattern %q is not an IP/CIDR rule", p)
		}
		ip, _, err := net.ParseCIDR(p)
		if err != nil || ip.To4() == nil {
			t.Errorf("seed pattern %q must be an IPv4 CIDR", p)
		}
	}
}

// Documented Google Meet media addresses must fall inside the seed, so the
// group is useful before the first feed fetch lands.
func TestGoogleMedia_SeedCoversMeetMediaIPs(t *testing.T) {
	g := FindByKey("google-media")
	if g == nil {
		t.Fatal("google-media group missing")
	}
	covered := func(ip string) bool {
		for _, p := range g.Patterns {
			if rules.MatchKey(p, ip) {
				return true
			}
		}
		return false
	}
	for _, ip := range []string{
		"74.125.250.1",  // Meet media
		"142.250.82.100", // Meet media
		"173.194.202.127",
		"209.85.130.1",
		"216.239.36.1",
	} {
		if !covered(ip) {
			t.Errorf("google-media seed must cover Meet media IP %s", ip)
		}
	}
	// Google Public DNS must stay out — same reason as the feed exclusion.
	if covered("8.8.8.8") {
		t.Error("google-media must not cover Google Public DNS")
	}
}
