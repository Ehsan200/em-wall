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
				"*.ai.com",
				"*.chatgpt.livekit.cloud",
			},
			Icon: svgBranded("O", "#0d0d0d", "#10a37f", "#ffffff"),
		},
		{
			Key:         "google-ai",
			DisplayName: "Google AI",
			Description: "Gemini, NotebookLM, AI Studio, AntiGravity + the Google backend hosts they call",
			Patterns: []string{
				// Product front-ends.
				"*.gemini.google.com",
				"*.notebooklm.google.com",
				"*.aistudio.google.com",
				"*.makersuite.google.com", // legacy AI Studio host, still redirected to
				"*.bard.google.com",
				"*.googleai.com",
				"*.ai.google.dev",
				"*.antigravity.app",
				"*.antigravity.com",
				"*.clients6.google.com",
				"*.generativelanguage.googleapis.com",
				"proactivebackend-pa.googleapis.com",
				"apis.google.com",
				"ogs.google.com",
				"*.googleusercontent.com",
			},
			Icon: svgBranded("G", "#4285f4", "#34a853", "#ffffff"),
		},
		{
			Key:         "google",
			DisplayName: "Google (all)",
			Description: "Search, Gmail, Drive/Docs, Maps, Photos, Accounts, Play + Google shared CDNs/APIs. Routes ALL of Google — broader than Google AI.",
			Patterns: []string{
				// Product front-ends + accounts (apex + every subdomain).
				"*.google.com",
				"*.gmail.com",
				"*.googlemail.com",
				// Shared backends / CDNs the products call.
				"*.googleapis.com",
				"*.gstatic.com",
				"*.googleusercontent.com",
				"*.ggpht.com",
				"*.gvt1.com",
				"*.gvt2.com",
				"*.gvt3.com",
				"*.goog", // pki.goog, *.gke.goog
				"*.googlesource.com",
				"*.recaptcha.net",
				// Shorteners / brand domains.
				"*.withgoogle.com",
				"*.goo.gl",
				"*.g.co",
				// Analytics / tag / ads (Google-owned).
				"*.google-analytics.com",
				"*.googletagmanager.com",
				"*.googletagservices.com",
				"*.googlesyndication.com",
				"*.googleadservices.com",
				"*.doubleclick.net",
			},
			Icon: svgGoogle(),
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
				"*.cursorapi.com",
				"*.cursor-cdn.com",
			},
			Icon: svgBranded("C", "#000000", "#cccccc", "#ffffff"),
		},
		{
			Key:         "perplexity",
			DisplayName: "Perplexity",
			Description: "Perplexity.ai search",
			Patterns: []string{
				"*.perplexity.ai",
				"*.pplx.ai",
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
				"*.hf.space",
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
			Key:         "grok",
			DisplayName: "Grok / xAI",
			Description: "Grok chat, xAI API/console, Grok-on-X surface + user content",
			Patterns: []string{
				"*.grok.com",
				"*.x.ai",
				"*.xai-cdn.com",
				"*.grokusercontent.com", // images/files Grok serves back (cf. oaiusercontent.com)
				"grok.x.com",            // Grok product surface reached via "sign in with X"
			},
			Icon: svgGrok(),
		},
		{
			Key:         "x",
			DisplayName: "X (Twitter)",
			Description: "X / Twitter web/app, link shortener, image CDN",
			Patterns: []string{
				"*.x.com",
				"*.twitter.com",
				"*.t.co",
				"*.twimg.com",
				"*.twttr.com",
				"*.twimg.org",
				"*.twimg.co",
			},
			Icon: svgX(),
		},
		{
			Key:         "telegram",
			DisplayName: "Telegram",
			Description: "Telegram web/app, Telegram CDN, t.me links, MTProto data-center IPs (Desktop)",
			Patterns: []string{
				"*.telegram.org",
				"*.telegram.me",
				"*.t.me",
				"*.tdesktop.com",
				"*.cdn-telegram.org",
				"*.telegram-cdn.org",
				"*.telesco.pe",
				"*.telegra.ph",
				"*.graph.org",
				// Telegram Desktop dials its MTProto data centers by hardcoded
				// IP and never issues a DNS query for them, so the domain
				// patterns above can't catch the app's core traffic. These are
				// Telegram's published DC ranges (AS62041/AS62014/AS59930);
				// as IP/CIDR route rules they pin the app through the proxy.
				// IPv4 only — v6 egress via the proxy utun isn't guaranteed.
				"149.154.160.0/20",
				"91.108.4.0/22",
				"91.108.8.0/22",
				"91.108.12.0/22",
				"91.108.16.0/22",
				"91.108.20.0/22",
				"91.108.56.0/22",
				"91.105.192.0/23",
				"95.161.64.0/20",
			},
			Icon: svgTelegram(),
		},
		{
			Key:         "meta",
			DisplayName: "Meta (Facebook / Instagram / WhatsApp)",
			Description: "Facebook, Instagram, WhatsApp, Messenger, Meta CDNs",
			Patterns: []string{
				"*.facebook.com",
				"*.facebook.net",
				"*.fbcdn.net",
				"*.fbsbx.com",
				"*.fb.com",
				"*.fb.gg",
				"*.fb.me",
				"*.facebookmail.com",
				"*.messenger.com",
				"*.m.me",
				"*.instagram.com",
				"*.cdninstagram.com",
				"*.igcdn.com",
				"*.ig.me",
				"*.whatsapp.com",
				"*.whatsapp.net",
				"*.wa.me",
				"*.meta.com",
				"*.threads.net",
				"*.threads.com",
			},
			Icon: svgMeta(),
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
				"youtubei.googleapis.com",
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
				"*.spotifycdn.net",
				"*.pscdn.co",
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
				"*.jetbrains.team",
				"*.jetbrains.dev",
				"*.jetbrains.org",
				"*.intellij.net",
				"*.intellij.com",
				"*.kotlinlang.org",
				"*.kotl.in",
				"*.jb.gg",
				"*.youtrack.cloud",
				"*.teamcity.com",
			},
			Icon: svgJetBrains(),
		},
		{
			Key:         "github",
			DisplayName: "GitHub",
			Description: "GitHub web/API, raw content, assets, Pages, GHCR",
			Patterns: []string{
				"*.github.com",
				"*.githubusercontent.com",
				"*.githubassets.com",
				"*.github.io",
				"*.github.dev",
				"*.githubapp.com",
				"*.ghcr.io",
			},
			Icon: svgGitHub(),
		},
		{
			Key:         "docker",
			DisplayName: "Docker",
			Description: "Docker Hub, registry, Desktop updates",
			Patterns: []string{
				"*.docker.com",
				"*.docker.io",
			},
			Icon: svgDocker(),
		},
		{
			Key:         "homebrew",
			DisplayName: "Homebrew",
			Description: "Homebrew formulae/taps + bottle downloads (GHCR / githubusercontent)",
			Patterns: []string{
				"*.brew.sh",
				"*.ghcr.io",
				"*.githubusercontent.com",
			},
			Icon: svgHomebrew(),
		},
		{
			Key:         "android-studio",
			DisplayName: "Android Studio",
			Description: "Android SDK/AOSP, Gradle, Maven Central",
			Patterns: []string{
				"*.android.com",
				"*.googlesource.com",
				"*.gradle.org",
				"*.gradle.com",
				"*.maven.org",
				"*.maven.apache.org",
				"*.sonatype.com",
				"maven.google.com",
				"dl.google.com",
			},
			Icon: svgAndroidStudio(),
		},
		{
			Key:         "notion",
			DisplayName: "Notion",
			Description: "Notion app/web, published sites, static + user-content CDNs",
			Patterns: []string{
				"*.notion.so",
				"*.notion.com",
				"*.notion.site",
				"*.notion-static.com",
				"*.notionusercontent.com",
				"*.makenotion.com",
				"*.cron.com", // Notion Calendar (formerly Cron)
			},
			Icon: svgNotion(),
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
				"*.browser-intake-datadoghq.com",
				"*.statsig.com",
				"*.statsigapi.net",
				"*.featuregates.org",
				"*.launchdarkly.com",
				"*.posthog.com",
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

// svgGoogle: white badge with the four-colour Google "G" (48x48 source
// paths, translated to centre inside the 64x64 badge).
func svgGoogle() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#ffffff" stroke="#dadce0" stroke-width="1.5"/>` +
		`<g transform="translate(8,8)">` +
		`<path fill="#4285F4" d="M45.12 24.5c0-1.56-.14-3.06-.4-4.5H24v8.51h11.84c-.51 2.75-2.06 5.08-4.39 6.64v5.52h7.11c4.16-3.83 6.56-9.47 6.56-16.17z"/>` +
		`<path fill="#34A853" d="M24 46c5.94 0 10.92-1.97 14.56-5.33l-7.11-5.52c-1.97 1.32-4.49 2.1-7.45 2.1-5.73 0-10.58-3.87-12.31-9.07H4.34v5.7C7.96 41.07 15.4 46 24 46z"/>` +
		`<path fill="#FBBC05" d="M11.69 28.18C11.25 26.86 11 25.45 11 24s.25-2.86.69-4.18v-5.7H4.34C2.85 17.09 2 20.45 2 24s.85 6.91 2.34 9.88l7.35-5.7z"/>` +
		`<path fill="#EA4335" d="M24 10.75c3.23 0 6.13 1.11 8.41 3.29l6.31-6.31C34.91 4.18 29.93 2 24 2 15.4 2 7.96 6.93 4.34 14.12l7.35 5.7c1.73-5.2 6.58-9.07 12.31-9.07z"/>` +
		`</g></svg>`
}

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

// svgGitHub: dark badge with the Octocat mark in white (16x16 source path
// scaled 2x and centred in the 64x64 viewBox).
func svgGitHub() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#181717"/>` +
		`<path transform="translate(16,16) scale(2)" fill="#ffffff" fill-rule="evenodd" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>`
}

// svgDocker: blue badge with the iconic container-stack on a whale shape.
func svgDocker() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#0db7ed"/>` +
		`<g fill="#ffffff">` +
		// top row (2 containers)
		`<rect x="22" y="20" width="8" height="8" rx="1"/>` +
		`<rect x="32" y="20" width="8" height="8" rx="1"/>` +
		// middle row (3 containers)
		`<rect x="12" y="30" width="8" height="8" rx="1"/>` +
		`<rect x="22" y="30" width="8" height="8" rx="1"/>` +
		`<rect x="32" y="30" width="8" height="8" rx="1"/>` +
		`<rect x="42" y="30" width="8" height="8" rx="1"/>` +
		// whale body curve underneath
		`<path d="M10 42 C 14 50 22 52 32 52 C 42 52 50 50 54 42 L 54 44 C 50 52 42 54 32 54 C 22 54 14 52 10 44 Z"/>` +
		`</g></svg>`
}

// svgHomebrew: amber badge with a beer-mug glyph (foam + body + handle).
func svgHomebrew() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#fbb040"/>` +
		`<g fill="#ffffff">` +
		// foam (three rounded blobs on top)
		`<circle cx="22" cy="18" r="4"/>` +
		`<circle cx="30" cy="16" r="5"/>` +
		`<circle cx="38" cy="18" r="4"/>` +
		// mug body
		`<rect x="18" y="22" width="22" height="28" rx="2"/>` +
		// handle
		`<path d="M40 28 h 4 a 6 6 0 0 1 0 12 h -4 v -3 h 4 a 3 3 0 0 0 0 -6 h -4 z"/>` +
		`</g>` +
		// darker amber stripes inside the mug to read as liquid
		`<g fill="#a06b00" opacity="0.35">` +
		`<rect x="20" y="32" width="18" height="2"/>` +
		`<rect x="20" y="38" width="18" height="2"/>` +
		`<rect x="20" y="44" width="18" height="2"/>` +
		`</g></svg>`
}

// svgX: black badge with the white X wordmark (post-rebrand Twitter logo).
func svgX() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#000000"/>` +
		`<path fill="#ffffff" d="M38.7 17h5.6L32.1 31.1 46.5 47h-11.3l-8.8-10.5L16.1 47H10.5l13.1-15.1L9.8 17h11.6l8 9.6L38.7 17zm-2 26.6h3.1L21.4 20.2H18.1l18.6 23.4z"/>` +
		`</svg>`
}

// svgGrok: black badge with the angular xAI/Grok slash mark in white.
func svgGrok() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#000000"/>` +
		`<g fill="#ffffff">` +
		`<path d="M18 46 L33 26 L37.5 31.5 L24 46 Z"/>` +
		`<path d="M30 22 L44 22 L44 46 L38 46 L38 31 L34.5 27 Z"/>` +
		`<rect x="38" y="40" width="6" height="6"/>` +
		`</g></svg>`
}

// svgTelegram: light-blue badge with a white paper-plane glyph.
func svgTelegram() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<defs><linearGradient id="tg" x1="0" y1="0" x2="0" y2="64" gradientUnits="userSpaceOnUse">` +
		`<stop offset="0" stop-color="#37bbfe"/>` +
		`<stop offset="1" stop-color="#007dbb"/></linearGradient></defs>` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="url(#tg)"/>` +
		// paper plane body
		`<path fill="#ffffff" d="M14 31 L50 17 L44 47 L33 39 L28 46 L26 38 Z"/>` +
		// inner fold (darker crease)
		`<path fill="#c8daea" d="M26 38 L44 22 L33 39 Z"/>` +
		`</svg>`
}

// svgMeta: gradient badge with the Meta infinity-loop mark in white.
func svgMeta() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<defs><linearGradient id="meta" x1="0" y1="64" x2="64" y2="0" gradientUnits="userSpaceOnUse">` +
		`<stop offset="0" stop-color="#0064e1"/>` +
		`<stop offset="0.5" stop-color="#7b2cd6"/>` +
		`<stop offset="1" stop-color="#f02849"/></linearGradient></defs>` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="url(#meta)"/>` +
		`<path fill="none" stroke="#ffffff" stroke-width="5" stroke-linecap="round" ` +
		`d="M14 40 C 14 26 22 22 28 28 C 32 32 36 38 42 38 C 50 38 52 30 50 25 C 48 21 42 21 38 26 C 32 33 28 41 22 41 C 16 41 14 36 14 40 Z"/>` +
		`</svg>`
}

// svgNotion: white badge with the black Notion "N" (angular serif N with
// the diagonal stroke, framed like the app icon).
func svgNotion() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#ffffff" stroke="#e6e6e6" stroke-width="1.5"/>` +
		`<g fill="#000000">` +
		// left vertical stem
		`<rect x="19" y="17" width="6" height="30"/>` +
		// right vertical stem
		`<rect x="39" y="17" width="6" height="30"/>` +
		// diagonal connecting the two stems
		`<path d="M19 17 L27 17 L45 41 L45 47 L37 47 L19 23 Z"/>` +
		`</g></svg>`
}

// svgAndroidStudio: dark navy badge with the green Android bugdroid head.
func svgAndroidStudio() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" preserveAspectRatio="xMidYMid meet">` +
		`<rect x="2" y="2" width="60" height="60" rx="14" ry="14" fill="#073042"/>` +
		`<g stroke="#3ddc84" stroke-width="2.5" stroke-linecap="round">` +
		// antennae
		`<line x1="24" y1="18" x2="27" y2="26"/>` +
		`<line x1="40" y1="18" x2="37" y2="26"/>` +
		`</g>` +
		// head (top half-circle + flat bottom)
		`<path fill="#3ddc84" d="M16 32 A 16 16 0 0 1 48 32 L 48 44 L 16 44 Z"/>` +
		// eyes
		`<circle cx="25" cy="34" r="2" fill="#073042"/>` +
		`<circle cx="39" cy="34" r="2" fill="#073042"/>` +
		// arms (short stubs on the sides)
		`<rect x="10" y="34" width="4" height="10" rx="2" fill="#3ddc84"/>` +
		`<rect x="50" y="34" width="4" height="10" rx="2" fill="#3ddc84"/>` +
		// legs
		`<rect x="22" y="46" width="4" height="8" rx="2" fill="#3ddc84"/>` +
		`<rect x="38" y="46" width="4" height="8" rx="2" fill="#3ddc84"/>` +
		`</svg>`
}
