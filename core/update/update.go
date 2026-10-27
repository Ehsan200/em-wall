// Package update asks GitHub for the latest em-wall release. Shared by the
// app (update banner, self-update) and the daemon (update prompt while the
// app is closed), so both agree on what "newer" means.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ReleasesAPI is the GitHub Releases endpoint for this project.
const ReleasesAPI = "https://api.github.com/repos/ehsan200/em-wall/releases/latest"

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // "sha256:<hex>" on recent releases
}

type Release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

// Latest fetches the latest published release. current is only used for
// the User-Agent.
func Latest(ctx context.Context, current string) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesAPI, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "em-wall/"+current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("releases api: %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Release{}, err
	}
	return r, nil
}

// DMG returns the release's .dmg asset, if it has one.
func (r Release) DMG() (Asset, bool) {
	for _, a := range r.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".dmg") {
			return a, true
		}
	}
	return Asset{}, false
}

// Newer reports whether latest is a higher version than current.
// Both strings may optionally start with "v". Non-numeric segments are
// treated as 0, so malformed tags lose gracefully instead of crashing.
func Newer(latest, current string) bool {
	trim := func(s string) string { return strings.TrimPrefix(s, "v") }
	lp := strings.SplitN(trim(latest), ".", 3)
	cp := strings.SplitN(trim(current), ".", 3)
	for i := 0; i < 3; i++ {
		lv, cv := 0, 0
		if i < len(lp) {
			lv, _ = strconv.Atoi(lp[i])
		}
		if i < len(cp) {
			cv, _ = strconv.Atoi(cp[i])
		}
		if lv > cv {
			return true
		}
		if lv < cv {
			return false
		}
	}
	return false
}
