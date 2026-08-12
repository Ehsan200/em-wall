package groups

// Categories bucket the curated registry for display. The quick-add bar in
// the UI renders one collapsible section per category instead of a single
// flat wall of ~40 cards, which stopped being scannable once the dev
// registries were added.
//
// Kept as a side table rather than a field on each Group literal so the
// registry entries stay focused on patterns; the DTO carries the resolved
// value to the frontend.
const (
	CategoryAI        = "AI"
	CategoryDev       = "Developer"
	CategoryPlatform  = "Platforms"
	CategorySocial    = "Social"
	CategoryMedia     = "Media"
	CategoryWork      = "Productivity"
	CategoryOther     = "Other"
	CategoryCustomKey = "Custom" // user-created groups; not in the table below
)

// CategoryOrder is the display order of the sections.
func CategoryOrder() []string {
	return []string{
		CategoryAI,
		CategoryDev,
		CategoryPlatform,
		CategorySocial,
		CategoryMedia,
		CategoryWork,
		CategoryOther,
	}
}

var categories = map[string]string{
	// AI products and coding assistants.
	"anthropic":      CategoryAI,
	"openai":         CategoryAI,
	"google-ai":      CategoryAI,
	"github-copilot": CategoryAI,
	"cursor":         CategoryAI,
	"perplexity":     CategoryAI,
	"huggingface":    CategoryAI,
	"mistral":        CategoryAI,
	"grok":           CategoryAI,

	// Developer tooling: source hosts, package registries, toolchains.
	"github":         CategoryDev,
	"docker":         CategoryDev,
	"homebrew":       CategoryDev,
	"jetbrains":      CategoryDev,
	"android-studio": CategoryDev,
	"python":         CategoryDev,
	"golang":         CategoryDev,
	"maven":          CategoryDev,
	"npm":            CategoryDev,
	"rust":           CategoryDev,
	"ruby":           CategoryDev,
	"dotnet":         CategoryDev,
	"php":            CategoryDev,
	"hashicorp":      CategoryDev,

	// Whole-vendor surfaces — broad by design.
	"google":       CategoryPlatform,
	"google-media": CategoryPlatform,

	"x":        CategorySocial,
	"telegram": CategorySocial,
	"meta":     CategorySocial,

	"youtube":    CategoryMedia,
	"spotify":    CategoryMedia,
	"soundcloud": CategoryMedia,

	"notion": CategoryWork,
	"figma":  CategoryWork,

	"telemetry-common": CategoryOther,
}

// Category returns the display category for a curated group key, or
// CategoryOther when the key has no entry (a new group added without
// touching this table still renders, just in the catch-all section).
func Category(key string) string {
	if c, ok := categories[key]; ok {
		return c
	}
	return CategoryOther
}
