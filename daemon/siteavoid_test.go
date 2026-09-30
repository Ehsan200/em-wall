package main

import (
	"io"
	"log"
	"reflect"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
)

func TestSiteAvoid(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	a := newSiteAvoid()
	a.now = func() time.Time { return now }
	keys := func(n string) []string { return []string{n} }
	ranked := []string{"a", "b", "c"}

	// One strike is not enough.
	if a.strike("site:x.com", "a") {
		t.Fatal("avoided after one strike")
	}
	if got := a.demote("site:x.com", ranked, keys); !reflect.DeepEqual(got, ranked) {
		t.Fatalf("demoted after one strike: %v", got)
	}
	// The second, within the window, moves a behind the others — for this
	// site only.
	if !a.strike("site:x.com", "a") {
		t.Fatal("not avoided after two strikes")
	}
	if got := a.demote("site:x.com", ranked, keys); !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Fatalf("demote = %v, want [b c a]", got)
	}
	if got := a.demote("site:y.com", ranked, keys); !reflect.DeepEqual(got, ranked) {
		t.Fatalf("other site affected: %v", got)
	}
	// Carrying the site again clears it.
	a.clear("site:x.com", "a")
	if got := a.demote("site:x.com", ranked, keys); !reflect.DeepEqual(got, ranked) {
		t.Fatalf("still demoted after clear: %v", got)
	}
	// Strikes spread wider than the window don't add up.
	a.strike("site:x.com", "b")
	now = now.Add(siteAvoidWindow + time.Second)
	if a.strike("site:x.com", "b") {
		t.Fatal("strikes outside the window added up")
	}
	// Avoidance expires.
	a.strike("site:x.com", "b")
	now = now.Add(siteAvoidFor + time.Second)
	if got := a.demote("site:x.com", ranked, keys); !reflect.DeepEqual(got, ranked) {
		t.Fatalf("avoidance outlived siteAvoidFor: %v", got)
	}
	// Every member avoided: ranking used as is.
	for _, n := range ranked {
		a.strike("site:z.com", n)
		a.strike("site:z.com", n)
	}
	if got := a.demote("site:z.com", ranked, keys); !reflect.DeepEqual(got, ranked) {
		t.Fatalf("all avoided = %v, want ranking untouched", got)
	}
}

// A ban is on the exit address: a member that never failed the site is
// avoided too when it leaves through the exit that did.
func TestSiteAvoidSharedExit(t *testing.T) {
	pf := &proxyForwarder{sticky: newStickyBindings(), avoid: newSiteAvoid(), exits: &exitKeys{}, sampler: newLogSampler()}
	pf.exits.set(map[string]string{"a": "1.2.3.4", "b": "1.2.3.4", "c": "5.6.7.8"})
	entry := proxy.Entry{Hostname: "open.spotify.com", ProxyNames: []string{"a", "b", "c"}}
	pf.logger = log.New(io.Discard, "", 0)

	pf.noteUpstreamFailure(entry, "a")
	pf.noteUpstreamFailure(entry, "a")
	if got := pf.orderedNames(entry); !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Fatalf("order = %v, want c first (a and b share the banned exit)", got)
	}
	// Another host of the same site gets the same verdict.
	other := proxy.Entry{Hostname: "api.spotify.com", ProxyNames: []string{"a", "b", "c"}}
	if got := pf.orderedNames(other); got[0] != "c" {
		t.Fatalf("sibling host order = %v, want c first", got)
	}
	// b carrying the site proves the exit works for it again.
	pf.noteSiteCarried(entry, "b")
	if got := pf.orderedNames(entry); !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Fatalf("after b carried it: %v, want [b c a]", got)
	}
}
