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
	"google-media":     "#00832d", // Google Meet green
	"microsoft":        "#00a4ef", // Microsoft blue (one of the four logo squares)
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
	"pandora":          "#3668ff", // Pandora blue
	"airbnb":           "#ff5a5f", // Airbnb coral
	"booking":          "#0071c2", // Booking.com blue (lightened from #003580 for dark bg)
	"jetbrains":        "#ff318c", // JetBrains magenta
	"github":           "#e6edf3", // GitHub near-white
	"docker":           "#2496ed", // Docker blue
	"homebrew":         "#fbb040", // Homebrew amber
	"python":           "#3776ab", // Python blue
	"golang":           "#00add8", // Go cyan
	"maven":            "#c71a36", // Maven red
	"npm":              "#cb3837", // npm red
	"rust":             "#dea584", // Rust tan (black brand, lightened for dark bg)
	"ruby":             "#cc342d", // Ruby red
	"dotnet":           "#a67bff", // .NET purple (lightened from #512bd4 for dark bg)
	"php":              "#777bb4", // PHP indigo
	"hashicorp":        "#a77bff", // Terraform purple (lightened for dark bg)
	"android-studio":   "#3ddc84", // Android green
	"telemetry-common": "#6c5ce7", // generic telemetry purple
}

// BrandColor returns the group's brand color hex, or "" if the key has no
// assigned color (the UI falls back to a rotating palette).
func BrandColor(key string) string { return brandColors[key] }

// GroupForKey returns the first known group that owns key — a hostname or a
// destination IP string — matching each group's patterns via rules.MatchKey
// (`*.x.com` covers the apex and any subdomain; an IP/CIDR pattern covers a
// contained IP key). Groups are tried in KnownGroups order, so earlier (more
// specific product) groups win over broader ones. Returns ok=false when no
// group claims the key. This is what attributes IP-routed traffic (e.g.
// Telegram MTProto DC IPs) to its group on the usage dashboard.
func GroupForKey(key string) (gkey, display string, ok bool) {
	for _, g := range KnownGroups() {
		for _, pat := range g.Patterns {
			if rules.MatchKey(pat, key) {
				return g.Key, g.DisplayName, true
			}
		}
	}
	return "", "", false
}

// GroupForDomain is GroupForKey specialised to hostnames; kept for callers
// that only ever pass a DNS name.
func GroupForDomain(host string) (key, display string, ok bool) {
	return GroupForKey(host)
}
