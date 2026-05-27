// Package groups holds predefined collections of domain patterns for
// well-known services. Used by the UI to one-click create N rules
// covering an entire service ("Claude / Anthropic", "OpenAI", …)
// instead of typing each pattern manually.
//
// Patterns use the same wildcard semantics as the rule engine
// (rules.Match): `*.x.com` matches the apex `x.com` and any
// subdomain. So we only need one entry per top-level domain.
package groups

type Group struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Icon        string   `json:"icon"` // inline SVG
}

// KnownGroups returns the curated registry. Order is the recommended
// display order in the UI.
func KnownGroups() []Group {
	return []Group{
		{
			Key:         "anthropic",
			DisplayName: "Claude / Anthropic",
			Description: "Claude (chat), Anthropic API, Console, Workbench",
			Patterns: []string{
				"*.anthropic.com",
				"*.claude.ai",
				"*.claude.com",
			},
			Icon: svgBranded("A", "#d97757", "#fef2e8", "#ffffff"),
		},
		{
			Key:         "openai",
			DisplayName: "OpenAI / ChatGPT / Codex",
			Description: "ChatGPT, OpenAI API, Codex, platform, Sora, DALL·E",
			Patterns: []string{
				"*.openai.com",
				"*.chatgpt.com",
				"*.oaistatic.com",
				"*.oaiusercontent.com",
				"*.sora.com",
				"*.openai.azure.com",
			},
			Icon: svgBranded("O", "#0d0d0d", "#10a37f", "#ffffff"),
		},
		{
			Key:         "google-ai",
			DisplayName: "Google AI",
			Description: "Gemini, NotebookLM, AI Studio, AntiGravity, generativelanguage",
			Patterns: []string{
				"*.gemini.google.com",
				"*.notebooklm.google.com",
				"*.aistudio.google.com",
				"*.bard.google.com",
				"*.googleai.com",
				"*.ai.google.dev",
				"*.generativelanguage.googleapis.com",
				"*.antigravity.app",
				"*.antigravity.com",
			},
			Icon: svgBranded("G", "#4285f4", "#34a853", "#ffffff"),
		},
		{
			Key:         "github-copilot",
			DisplayName: "GitHub Copilot",
			Description: "Copilot Chat + suggestion endpoints",
			Patterns: []string{
				"*.githubcopilot.com",
				"*.individual.githubcopilot.com",
				"*.business.githubcopilot.com",
				"*.copilot.github.com",
			},
			Icon: svgBranded("Co", "#0d1117", "#7ee787", "#ffffff"),
		},
		{
			Key:         "cursor",
			DisplayName: "Cursor",
			Description: "Cursor AI editor endpoints",
			Patterns: []string{
				"*.cursor.sh",
				"*.cursor.com",
				"*.cursor.so",
			},
			Icon: svgBranded("C", "#000000", "#cccccc", "#ffffff"),
		},
		{
			Key:         "perplexity",
			DisplayName: "Perplexity",
			Description: "Perplexity.ai search",
			Patterns: []string{
				"*.perplexity.ai",
			},
			Icon: svgBranded("P", "#22b8cd", "#0a4f5e", "#ffffff"),
		},
		{
			Key:         "huggingface",
			DisplayName: "Hugging Face",
			Description: "Hugging Face hub, inference, datasets",
			Patterns: []string{
				"*.huggingface.co",
				"*.hf.co",
			},
			Icon: svgBranded("HF", "#ffd21e", "#ff9d00", "#000000"),
		},
		{
			Key:         "mistral",
			DisplayName: "Mistral AI",
			Description: "Mistral chat + API",
			Patterns: []string{
				"*.mistral.ai",
			},
			Icon: svgBranded("M", "#ff7000", "#ffd200", "#ffffff"),
		},
		{
			Key:         "youtube",
			DisplayName: "YouTube",
			Description: "YouTube web/app, video CDN, thumbnails",
			Patterns: []string{
				"*.youtube.com",
				"*.youtu.be",
				"*.youtube-nocookie.com",
				"*.googlevideo.com",
				"*.ytimg.com",
				"*.ggpht.com",
			},
			Icon: svgYouTube(),
		},
		{
			Key:         "spotify",
			DisplayName: "Spotify",
			Description: "Spotify app/web, audio + image CDN",
			Patterns: []string{
				"*.spotify.com",
				"*.scdn.co",
				"*.spotifycdn.com",
				"*.spoti.fi",
			},
			Icon: svgSpotify(),
		},
		{
			Key:         "soundcloud",
			DisplayName: "SoundCloud",
			Description: "SoundCloud web/app + media CDN",
			Patterns: []string{
				"*.soundcloud.com",
				"*.sndcdn.com",
			},
			Icon: svgSoundCloud(),
		},
		{
			Key:         "jetbrains",
			DisplayName: "JetBrains",
			Description: "IDEs, Toolbox, account, plugins, AI Assistant",
			Patterns: []string{
				"*.jetbrains.com",
				"*.jetbrains.space",
				"*.jetbrains.ai",
				"*.intellij.net",
			},
			Icon: svgJetBrains(),
		},
		{
			Key:         "telemetry-common",
			DisplayName: "Common telemetry / analytics",
			Description: "Sentry, Mixpanel, Segment, Amplitude, Datadog browser",
			Patterns: []string{
				"*.sentry.io",
				"*.ingest.sentry.io",
				"*.mixpanel.com",
				"*.segment.io",
				"*.segment.com",
				"*.amplitude.com",
				"*.datadoghq.com",
				"*.datadoghq.eu",
			},
			Icon: svgBranded("T", "#6c5ce7", "#a29bfe", "#ffffff"),
		},
	}
}

// FindByKey returns the group with the given key, or nil.
func FindByKey(key string) *Group {
	for i, g := range KnownGroups() {
		if g.Key == key {
			list := KnownGroups()
			return &list[i]
		}
	}
	return nil
}

// svgBranded returns an inline SVG with the given initials over a
// rounded square in the brand colour. Same shape as the app icon
// fallback so groups and apps look visually consistent.
func svgBranded(initials, fill, accent, text string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="` + fill + `" stroke="` + accent + `" stroke-width="2"/>` +
		`<text x="32" y="42" font-family="Helvetica,Arial,sans-serif" font-size="22" font-weight="700" ` +
		`text-anchor="middle" fill="` + text + `">` + initials + `</text></svg>`
}

// The icon builders below produce recognizable brand glyphs framed in the
// same rounded-square badge as svgBranded, so all groups stay visually
// consistent. Any of them can be overridden by dropping a real
// icons/<key>.svg|.png|.ico file (see LoadIcon).

// svgYouTube: red badge with a white play triangle.
func svgYouTube() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#ff0000"/>` +
		`<path d="M25 21 L46 32 L25 43 Z" fill="#ffffff"/></svg>`
}

// svgSpotify: green badge with the three black "sound" arcs.
func svgSpotify() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#1db954"/>` +
		`<g fill="none" stroke="#0d1b12" stroke-linecap="round">` +
		`<path d="M17 25c11-3.5 22-2 30 2.4" stroke-width="5"/>` +
		`<path d="M18.5 34c9-2.5 18.5-1 25 2.3" stroke-width="4"/>` +
		`<path d="M20 42.5c7.5-2 14.5-1 20 1.4" stroke-width="3"/></g></svg>`
}

// svgSoundCloud: orange badge with a white waveform.
func svgSoundCloud() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#ff5500"/>` +
		`<g fill="#ffffff">` +
		`<rect x="15" y="29" width="4" height="11" rx="2"/>` +
		`<rect x="23" y="23" width="4" height="17" rx="2"/>` +
		`<rect x="31" y="19" width="4" height="21" rx="2"/>` +
		`<rect x="39" y="25" width="4" height="15" rx="2"/>` +
		`<rect x="47" y="31" width="4" height="9" rx="2"/></g></svg>`
}

// svgJetBrains: gradient badge with the "JB" wordmark and underline.
func svgJetBrains() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<defs><linearGradient id="jb" x1="0" y1="64" x2="64" y2="0">` +
		`<stop offset="0" stop-color="#fe315d"/>` +
		`<stop offset="0.55" stop-color="#f97a12"/>` +
		`<stop offset="1" stop-color="#b24cf0"/></linearGradient></defs>` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="url(#jb)"/>` +
		`<text x="32" y="34" font-family="Helvetica,Arial,sans-serif" font-size="19" font-weight="800" ` +
		`text-anchor="middle" fill="#ffffff">JB</text>` +
		`<rect x="18" y="44" width="20" height="4" rx="1" fill="#ffffff"/></svg>`
}
