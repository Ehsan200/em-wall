package groups

import "github.com/ehsan/em-wall/core/rules"

// brandColors maps each group key to its brand accent color (hex), used to
// paint that group — and every domain belonging to it — consistently in
// the UI. Picked to be the recognisable brand hue while staying legible on
// the app's dark chart background (pure-black brands are lightened).
var brandColors = map[string]string{
	"anthropic":        "#d97757", // Claude orange
	"openai":           "#10a37f", // OpenAI green
	"google-ai":        "#4285f4", // Google blue
	"google":           "#ea4335", // Google red
	"github-copilot":   "#8957e5", // Copilot purple
	"cursor":           "#b6b6c2", // Cursor neutral (black brand, lightened for dark bg)
	"perplexity":       "#22b8cd", // Perplexity teal
	"huggingface":      "#ffd21e", // HF yellow
	"mistral":          "#ff7000", // Mistral orange
	"grok":             "#e7e9ea", // xAI / Grok near-white
	"x":                "#1d9bf0", // X blue
	"telegram":         "#229ed9", // Telegram blue
	"meta":             "#0866ff", // Meta blue
	"youtube":          "#ff0000", // YouTube red
	"spotify":          "#1db954", // Spotify green
	"soundcloud":       "#ff5500", // SoundCloud orange
	"jetbrains":        "#ff318c", // JetBrains magenta
	"github":           "#e6edf3", // GitHub near-white
	"docker":           "#2496ed", // Docker blue
	"homebrew":         "#fbb040", // Homebrew amber
	"android-studio":   "#3ddc84", // Android green
	"telemetry-common": "#6c5ce7", // generic telemetry purple
}

// BrandColor returns the group's brand color hex, or "" if the key has no
// assigned color (the UI falls back to a rotating palette).
func BrandColor(key string) string { return brandColors[key] }

// GroupForDomain returns the first known group that owns host, matching
// each group's patterns with the same wildcard semantics as the rule
// engine (rules.Match: `*.x.com` matches the apex and any subdomain).
// Groups are tried in KnownGroups order, so earlier (more specific
// product) groups win over broader ones. Returns ok=false when no group
// claims the host.
func GroupForDomain(host string) (key, display string, ok bool) {
	for _, g := range KnownGroups() {
		for _, pat := range g.Patterns {
			if rules.Match(pat, host) {
				return g.Key, g.DisplayName, true
			}
		}
	}
	return "", "", false
}
