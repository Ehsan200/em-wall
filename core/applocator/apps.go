// Package applocator binds firewall rules to VPN/tunneling apps
// instead of to specific utun interfaces. It maintains the live
// app→utun mapping (via lsof) so that rules survive reconnects that
// change the utun number.
package applocator

import (
	"os"
	"strings"
)

// App describes a known VPN / tunneling client.
//
// `Processes` are case-insensitive substrings matched against the
// process column from `lsof -nP`. `BundlePath` is the primary install
// location (used by the UI for display); `BundlePathCandidates` is
// the full set of paths we'll probe when checking IsInstalled() —
// some apps ship under several different names depending on version
// (e.g. AnyConnect → Cisco Secure Client → AnyConnect).
type App struct {
	Key                  string   `json:"key"`
	DisplayName          string   `json:"displayName"`
	BundleID             string   `json:"bundleId"`
	BundlePath           string   `json:"bundlePath"`
	BundlePathCandidates []string `json:"-"`
	Processes            []string `json:"-"`
	FallbackSVG          string   `json:"-"` // inline SVG when icon extraction fails
}

// allBundlePaths returns BundlePath followed by any extra candidates,
// deduped. Order matters — IsInstalled / icon extraction take the
// first hit.
func (a App) allBundlePaths() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 1+len(a.BundlePathCandidates))
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(a.BundlePath)
	for _, p := range a.BundlePathCandidates {
		add(p)
	}
	return out
}

// InstalledPath returns the first candidate bundle path that exists
// on disk, or "" if none do.
func (a App) InstalledPath() string {
	for _, p := range a.allBundlePaths() {
		st, err := os.Stat(p)
		if err == nil && st.IsDir() {
			return p
		}
	}
	return ""
}

// IsInstalled reports whether any candidate bundle path exists.
func (a App) IsInstalled() bool {
	return a.InstalledPath() != ""
}

// MatchesProcess reports whether procName looks like one of this
// app's expected process names.
func (a App) MatchesProcess(procName string) bool {
	low := strings.ToLower(procName)
	for _, p := range a.Processes {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// KnownApps returns the static registry. Order is the recommended
// display order in the UI.
func KnownApps() []App {
	white := "#ffffff"
	return []App{
		{
			Key:         "v2box",
			DisplayName: "v2box",
			BundleID:    "com.helloworld.v2box",
			BundlePath:  "/Applications/v2box.app",
			Processes:   []string{"v2box"},
			FallbackSVG: svgBranded("V2", "#1f6feb", "#74c0fc", white),
		},
		{
			Key:         "v2raytun",
			DisplayName: "V2RayTun",
			BundleID:    "com.v2raytun.macos",
			BundlePath:  "/Applications/V2RayTun.app",
			Processes:   []string{"v2raytun"},
			FallbackSVG: svgBranded("V2T", "#0f4c81", "#74c0fc", white),
		},
		{
			Key:         "hiddify",
			DisplayName: "Hiddify",
			BundleID:    "app.hiddify.com",
			BundlePath:  "/Applications/Hiddify.app",
			Processes:   []string{"hiddify"},
			FallbackSVG: svgBranded("Hd", "#7c3aed", "#c4b5fd", white),
		},
		{
			Key:         "streisand",
			DisplayName: "Streisand",
			BundleID:    "me.proxypin.streisand",
			BundlePath:  "/Applications/Streisand.app",
			Processes:   []string{"streisand"},
			FallbackSVG: svgBranded("Sd", "#0ea5e9", "#7dd3fc", white),
		},
		{
			Key:         "tailscale",
			DisplayName: "Tailscale",
			BundleID:    "io.tailscale.ipn.macos",
			BundlePath:  "/Applications/Tailscale.app",
			Processes:   []string{"tailscaled", "tailscale"},
			FallbackSVG: svgBranded("Ts", "#1c1c1c", "#ffffff", white),
		},
		{
			Key:         "wireguard",
			DisplayName: "WireGuard",
			BundleID:    "com.wireguard.macos",
			BundlePath:  "/Applications/WireGuard.app",
			Processes:   []string{"wireguard-go", "wg-quick", "wireguard"},
			FallbackSVG: svgBranded("Wg", "#a91d3a", "#fda4af", white),
		},
		{
			Key:         "openvpn",
			DisplayName: "OpenVPN",
			BundleID:    "net.openvpn.client",
			BundlePath:  "/Applications/OpenVPN Connect.app",
			BundlePathCandidates: []string{
				"/Applications/Tunnelblick.app",
				"/Applications/Viscosity.app",
			},
			Processes:   []string{"openvpn", "tunnelblick", "viscosity"},
			FallbackSVG: svgBranded("OV", "#ea580c", "#fdba74", white),
		},
		{
			Key:         "warp",
			DisplayName: "Cloudflare WARP",
			BundleID:    "com.cloudflare.1dot1dot1dot1.macos",
			BundlePath:  "/Applications/Cloudflare WARP.app",
			Processes:   []string{"warp-svc", "cloudflarewarp"},
			FallbackSVG: svgBranded("WP", "#f38020", "#fdba74", white),
		},
		{
			Key:         "nordvpn",
			DisplayName: "NordVPN",
			BundleID:    "com.nordvpn.osx",
			BundlePath:  "/Applications/NordVPN.app",
			Processes:   []string{"nordvpn"},
			FallbackSVG: svgBranded("Nd", "#4687ff", "#a3c0ff", white),
		},
		{
			Key:         "expressvpn",
			DisplayName: "ExpressVPN",
			BundleID:    "com.expressvpn.ExpressVPN",
			BundlePath:  "/Applications/ExpressVPN.app",
			Processes:   []string{"expressvpn"},
			FallbackSVG: svgBranded("Ex", "#dc2626", "#fca5a5", white),
		},
		{
			Key:         "protonvpn",
			DisplayName: "Proton VPN",
			BundleID:    "ch.protonvpn.mac",
			BundlePath:  "/Applications/Proton VPN.app",
			Processes:   []string{"protonvpn"},
			FallbackSVG: svgBranded("Pr", "#6d4aff", "#c4b5fd", white),
		},
		{
			Key:         "mullvad",
			DisplayName: "Mullvad",
			BundleID:    "net.mullvad.vpn",
			BundlePath:  "/Applications/Mullvad VPN.app",
			Processes:   []string{"mullvad"},
			FallbackSVG: svgBranded("Ml", "#44475a", "#f1c40f", white),
		},
		{
			Key:         "globalprotect",
			DisplayName: "GlobalProtect",
			BundleID:    "com.paloaltonetworks.GlobalProtect",
			BundlePath:  "/Applications/GlobalProtect.app",
			Processes:   []string{"globalprotect"},
			FallbackSVG: svgBranded("Gp", "#0079a1", "#7dd3fc", white),
		},
		{
			Key:         "anyconnect",
			DisplayName: "Cisco AnyConnect",
			BundleID:    "com.cisco.anyconnect.gui",
			BundlePath:  "/Applications/AnyConnect.app",
			BundlePathCandidates: []string{
				"/Applications/Cisco/Cisco Secure Client.app",
				"/Applications/Cisco/Cisco AnyConnect Secure Mobility Client.app",
				"/Applications/Cisco AnyConnect Secure Mobility Client.app",
			},
			Processes:   []string{"anyconnect", "cisco_secure_client", "vpnagent", "vpnui"},
			FallbackSVG: svgBranded("Cs", "#1ba0d7", "#9bdaff", white),
		},
		{
			Key:         "clashx",
			DisplayName: "ClashX",
			BundleID:    "com.west2online.ClashX",
			BundlePath:  "/Applications/ClashX.app",
			BundlePathCandidates: []string{
				"/Applications/ClashX Pro.app",
				"/Applications/Clash Verge.app",
			},
			Processes:   []string{"clashx", "clash"},
			FallbackSVG: svgBranded("Cx", "#10b981", "#6ee7b7", white),
		},
	}
}

// FindByKey returns the app with the given key, or nil.
func FindByKey(key string) *App {
	for i, a := range KnownApps() {
		if a.Key == key {
			apps := KnownApps()
			return &apps[i]
		}
	}
	return nil
}

// svgBranded returns an SVG with the given initials over a brand-color
// rounded square. Used as fallback when the app's bundle isn't on
// disk so we can't extract its real .icns. Colors are picked to
// roughly evoke each app's branding.
func svgBranded(initials, fill, stroke, text string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="` + fill + `" stroke="` + stroke + `" stroke-width="2"/>` +
		`<text x="32" y="42" font-family="Helvetica,Arial,sans-serif" font-size="24" font-weight="700" ` +
		`text-anchor="middle" fill="` + text + `">` + initials + `</text></svg>`
}
