package main

import (
	"testing"

	"github.com/ehsan/em-wall/core/selfuse"
)

// The self-update endpoint must be in em-wall's own-hostname table, or
// its lookups would count as the user's in group suggestions.
func TestReleasesAPIIsSelfHost(t *testing.T) {
	h := selfuse.HostOf(releasesAPI)
	for _, p := range selfuse.NewSet(selfuse.Static()).Purposes(h) {
		if p == selfuse.PurposeAppUpdate {
			return
		}
	}
	t.Errorf("%s (releasesAPI) is not listed as an app-update host", h)
}
