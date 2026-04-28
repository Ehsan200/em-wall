package applocator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// IconResult is what callers get back: either PNG bytes (extracted
// from the installed bundle) or an inline SVG fallback.
type IconResult struct {
	MIME      string // "image/png" or "image/svg+xml"
	Data      []byte
	Installed bool
}

type iconCache struct {
	mu    sync.Mutex
	cache map[string]IconResult
	dir   string // temp dir for extracted PNGs
}

func newIconCache() *iconCache {
	dir, _ := os.MkdirTemp("", "em-wall-icons-")
	return &iconCache{cache: map[string]IconResult{}, dir: dir}
}

var defaultIconCache = newIconCache()

// LoadIcon returns the icon for an app. Hot path: served from cache.
// First miss for an installed app extracts the .icns to PNG via sips.
// On any failure (or app not installed) returns the SVG fallback.
func LoadIcon(a App) IconResult {
	return defaultIconCache.load(a)
}

func (c *iconCache) load(a App) IconResult {
	c.mu.Lock()
	if r, ok := c.cache[a.Key]; ok {
		c.mu.Unlock()
		return r
	}
	c.mu.Unlock()

	r := c.extract(a)
	c.mu.Lock()
	c.cache[a.Key] = r
	c.mu.Unlock()
	return r
}

// InvalidateAll forces fresh icon extraction next call. Useful when
// the user installs/uninstalls an app while the daemon is running.
func InvalidateAll() { defaultIconCache.invalidate() }

func (c *iconCache) invalidate() {
	c.mu.Lock()
	c.cache = map[string]IconResult{}
	c.mu.Unlock()
}

func (c *iconCache) extract(a App) IconResult {
	bundle := a.InstalledPath()
	if bundle == "" {
		return c.fallback(a)
	}
	icns, err := findIcnsFile(bundle)
	if err != nil || icns == "" {
		return c.fallback(a)
	}
	out := filepath.Join(c.dir, a.Key+".png")
	cmd := exec.Command("/usr/bin/sips", "-Z", "128", "-s", "format", "png", icns, "--out", out)
	if err := cmd.Run(); err != nil {
		return c.fallback(a)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return c.fallback(a)
	}
	return IconResult{MIME: "image/png", Data: data, Installed: true}
}

func (c *iconCache) fallback(a App) IconResult {
	return IconResult{
		MIME:      "image/svg+xml",
		Data:      []byte(a.FallbackSVG),
		Installed: a.IsInstalled(),
	}
}

// findIcnsFile locates the primary .icns file inside a .app bundle.
// Strategy:
//  1. Try CFBundleIconFile from Info.plist via `defaults read`.
//  2. If that fails, fall back to the first .icns file in
//     Contents/Resources/.
func findIcnsFile(bundlePath string) (string, error) {
	infoPlist := filepath.Join(bundlePath, "Contents", "Info.plist")
	if _, err := os.Stat(infoPlist); err == nil {
		// `defaults read` strips quotes; the value may or may not have
		// the .icns extension.
		out, err := exec.Command("/usr/bin/defaults", "read", strings.TrimSuffix(infoPlist, ".plist"), "CFBundleIconFile").Output()
		if err == nil {
			name := strings.TrimSpace(string(out))
			if name != "" {
				if !strings.HasSuffix(strings.ToLower(name), ".icns") {
					name += ".icns"
				}
				candidate := filepath.Join(bundlePath, "Contents", "Resources", name)
				if _, err := os.Stat(candidate); err == nil {
					return candidate, nil
				}
			}
		}
	}
	// Fallback: first .icns in Resources.
	resDir := filepath.Join(bundlePath, "Contents", "Resources")
	entries, err := os.ReadDir(resDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".icns") {
			return filepath.Join(resDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .icns found in %s", resDir)
}
