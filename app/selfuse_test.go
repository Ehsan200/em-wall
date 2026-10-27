package main

import (
	"testing"

	"github.com/ehsan/em-wall/core/selfuse"
	"github.com/ehsan/em-wall/core/update"
)

// The self-update endpoint must be in em-wall's own-hostname table, or
// its lookups would count as the user's in group suggestions.
func TestReleasesAPIIsSelfHost(t *testing.T) {
	h := selfuse.HostOf(update.ReleasesAPI)
	for _, p := range selfuse.NewSet(selfuse.Static()).Purposes(h) {
		if p == selfuse.PurposeAppUpdate {
			return
		}
	}
	t.Errorf("%s (update.ReleasesAPI) is not listed as an app-update host", h)
}
