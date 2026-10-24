// Package selfuse lists the hostnames em-wall itself resolves, each with
// what it is used for. The daemon can't tell its own lookups from the
// user's, so group suggestions don't count these exact names, and the UI
// warns when a group covers one ("routing this group also routes em-wall's
// geo data updates").
//
// Only the static part lives here. The daemon adds the dynamic part —
// subscription URL hosts, proxy / xray server hosts, pool node hosts —
// from its stores (daemon/selfhosts.go).
package selfuse

import (
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/xray"
)

// Purposes.
const (
	PurposeExitIP       = "exit IP check"
	PurposeGeoData      = "geo data updates"
	PurposeAppUpdate    = "app updates"
	PurposeProbe        = "proxy health probes"
	PurposeSubscription = "subscription refresh"
	PurposeProxyServer  = "proxy server"
)

// Use is one hostname em-wall resolves and why.
type Use struct {
	Host    string
	Purpose string
}

// ExitIPHosts are the daemon's exit-IP probers (daemon/exitip.go
// exitProbers; a daemon test keeps the two in step).
var ExitIPHosts = []string{"ip-api.com", "ipwho.is", "get.geojs.io"}

// AppUpdateURLs are what the app's self-update fetches: the releases API
// (app/app.go releasesAPI) and the release download, which GitHub
// redirects to its asset hosts.
var AppUpdateURLs = []string{
	"https://api.github.com/",
	"https://github.com/",
	"https://objects.githubusercontent.com/",
	"https://release-assets.githubusercontent.com/",
}

// githubAssetHosts serve GitHub release downloads after the redirect —
// every geo source on GitHub lands on one of them.
var githubAssetHosts = []string{
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",
	"raw.githubusercontent.com",
}

// Static returns the hostnames fixed in code, derived from the real URLs
// where they are exported.
func Static() []Use {
	var out []Use
	add := func(purpose string, hosts ...string) {
		for _, h := range hosts {
			if h = HostOf(h); h != "" {
				out = append(out, Use{Host: h, Purpose: purpose})
			}
		}
	}
	add(PurposeExitIP, ExitIPHosts...)
	for _, key := range []string{geo.SourceLoyalsoldier, geo.SourceChocolate4U, geo.SourceV2Fly} {
		if s, err := geo.ResolveSource(key, ""); err == nil {
			add(PurposeGeoData, s.GeoSiteURL, s.GeoIPURL)
		}
	}
	add(PurposeGeoData, geo.DBIPBase)
	add(PurposeGeoData, githubAssetHosts...)
	add(PurposeAppUpdate, AppUpdateURLs...)
	add(PurposeProbe, netprobe.URLTestHost, xray.DefaultProbeURL)
	return out
}

// HostOf returns the lower-cased hostname of a URL or bare host, "" for
// an IP literal or nothing usable (an IP is never a DNS query).
func HostOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	s = u.Hostname()
	s = strings.TrimSuffix(strings.ToLower(strings.Trim(s, "[]")), ".")
	if s == "" {
		return ""
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return ""
	}
	return s
}

// Set is host → purposes (sorted, distinct). The zero value is empty.
type Set map[string][]string

// NewSet indexes uses.
func NewSet(uses ...[]Use) Set {
	s := Set{}
	for _, list := range uses {
		for _, u := range list {
			s.Add(u.Host, u.Purpose)
		}
	}
	return s
}

// Add records one use of host.
func (s Set) Add(host, purpose string) {
	host = HostOf(host)
	if host == "" || purpose == "" {
		return
	}
	for _, p := range s[host] {
		if p == purpose {
			return
		}
	}
	ps := append(s[host], purpose)
	sort.Strings(ps)
	s[host] = ps
}

// Has reports whether em-wall resolves exactly name (no subdomains: other
// names of the same service are the user's).
func (s Set) Has(name string) bool {
	_, ok := s[strings.TrimSuffix(strings.ToLower(name), ".")]
	return ok
}

// Purposes returns what em-wall uses host for, nil if nothing.
func (s Set) Purposes(host string) []string { return s[strings.ToLower(host)] }
