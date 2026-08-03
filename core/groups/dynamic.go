package groups

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// A dynamic group's pattern list is not fixed in this file — it's refreshed
// from a feed the vendor publishes, because the underlying ranges change
// without notice. The Patterns baked into the Group stay as the seed: what
// the group covers before the first successful fetch (and after a fetch that
// fails), so a group is never empty and never depends on network access to
// be useful.

// FormatGoogleIPRanges is Google's published IP-range JSON shape, served at
// https://www.gstatic.com/ipranges/goog.json (all Google-announced prefixes)
// and .../cloud.json (the GCP-customer subset):
//
//	{"syncToken":"…","creationTime":"…",
//	 "prefixes":[{"ipv4Prefix":"8.8.4.0/24"},{"ipv6Prefix":"2001:4860::/32"}]}
const FormatGoogleIPRanges = "google-ipranges"

// DefaultDynamicInterval is how often a dynamic group re-fetches when its
// source doesn't set an interval. Vendor range lists change on the order of
// weeks; daily is frequent enough and costs one request.
const DefaultDynamicInterval = 24 * time.Hour

// DynamicSource describes where a group's live pattern list comes from.
// Parsing is pure (no network) so it stays testable; the daemon does the
// fetching and caches the result.
type DynamicSource struct {
	// URL is the feed to fetch.
	URL string `json:"url"`
	// ExcludeURL, when set, is a second feed whose prefixes are subtracted
	// from the first. For Google that's cloud.json — GCP customer ranges we
	// do NOT want to route, since third-party services live there.
	ExcludeURL string `json:"excludeUrl,omitempty"`
	// Format selects the parser.
	Format string `json:"format"`
	// Exclude lists prefixes to drop unconditionally, on top of ExcludeURL.
	Exclude []string `json:"exclude,omitempty"`
	// IPv4Only drops v6 prefixes. Routing v6 through the proxy utun isn't
	// guaranteed (same reasoning as the Telegram DC ranges).
	IPv4Only bool `json:"ipv4Only,omitempty"`
	// Interval overrides DefaultDynamicInterval.
	Interval time.Duration `json:"-"`
}

// EffectiveInterval is Interval, or DefaultDynamicInterval when unset.
func (s *DynamicSource) EffectiveInterval() time.Duration {
	if s == nil || s.Interval <= 0 {
		return DefaultDynamicInterval
	}
	return s.Interval
}

// Parse turns a fetched feed (and its optional exclusion feed) into a
// canonical, deduplicated, sorted CIDR pattern list.
//
// It deliberately errors on an empty result: a feed that changed shape would
// otherwise silently reduce the group to nothing, and the caller would then
// delete every rule the group had created.
func (s *DynamicSource) Parse(feed, excludeFeed []byte) ([]string, error) {
	if s == nil {
		return nil, fmt.Errorf("groups: nil dynamic source")
	}
	if s.Format != FormatGoogleIPRanges {
		return nil, fmt.Errorf("groups: unknown dynamic format %q", s.Format)
	}

	drop := map[string]struct{}{}
	for _, p := range s.Exclude {
		if c, ok := canonicalCIDR(p); ok {
			drop[c] = struct{}{}
		}
	}
	if len(excludeFeed) > 0 {
		ex, err := parseIPRangeFeed(excludeFeed, s.IPv4Only)
		if err != nil {
			return nil, fmt.Errorf("exclusion feed: %w", err)
		}
		for _, p := range ex {
			drop[p] = struct{}{}
		}
	}

	all, err := parseIPRangeFeed(feed, s.IPv4Only)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, p := range all {
		if _, skip := drop[p]; skip {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("groups: feed yielded no usable prefixes")
	}
	sort.Strings(out)
	return out, nil
}

type ipRangeFeed struct {
	Prefixes []struct {
		IPv4Prefix string `json:"ipv4Prefix"`
		IPv6Prefix string `json:"ipv6Prefix"`
	} `json:"prefixes"`
}

// parseIPRangeFeed decodes the Google ipranges shape into canonical CIDR
// strings, deduplicated. Malformed individual prefixes are skipped; a feed
// that doesn't decode at all is an error.
func parseIPRangeFeed(b []byte, v4Only bool) ([]string, error) {
	var f ipRangeFeed
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("groups: decode ip-range feed: %w", err)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(f.Prefixes))
	for _, p := range f.Prefixes {
		raw := strings.TrimSpace(p.IPv4Prefix)
		if raw == "" {
			if v4Only {
				continue
			}
			raw = strings.TrimSpace(p.IPv6Prefix)
		}
		c, ok := canonicalCIDR(raw)
		if !ok {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// canonicalCIDR normalizes a prefix so string comparison is meaningful
// (host bits cleared, no stray whitespace). Bare IPs are not accepted —
// a range feed that emits one is malformed.
func canonicalCIDR(s string) (string, bool) {
	_, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return n.String(), true
}
