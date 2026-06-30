package groups

import (
	"embed"
	"path/filepath"
	"strings"
)

// Real brand icons fetched once and bundled with the binary.
// `_` after the filename means "include in this directory only".
//
//go:embed icons
var iconFS embed.FS

// IconResult is what callers get back: either bytes from disk (ICO,
// PNG, or SVG file shipped with the binary) or the inline SVG
// fallback when no file is available.
type IconResult struct {
	MIME string
	Data []byte
}

// LoadIcon returns the icon for a group. Tries embedded files in
// order of preference (.svg → .png → .ico) before falling back to
// the inline branded SVG from the registry entry.
func LoadIcon(g Group) IconResult {
	for _, ext := range []string{".svg", ".png", ".ico"} {
		data, err := iconFS.ReadFile(filepath.Join("icons", g.Key+ext))
		if err != nil {
			continue
		}
		mime := mimeForExt(ext)
		return IconResult{MIME: mime, Data: data}
	}
	return IconResult{MIME: "image/svg+xml", Data: []byte(g.Icon)}
}

// GenericSVG builds a fallback badge for a custom (user-created) group that
// has no shipped icon: the group's initials over a rounded square in its
// accent color. color may be "" — a default purple is used then.
func GenericSVG(displayName, color string) string {
	if strings.TrimSpace(color) == "" {
		color = "#6c5ce7"
	}
	return svgBranded(initialsFor(displayName), color, color, "#ffffff")
}

// initialsFor returns up to two uppercase letters drawn from the first
// alphanumeric chars of the first two words of name.
func initialsFor(name string) string {
	var out []rune
	for _, word := range strings.Fields(name) {
		for _, r := range word {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				out = append(out, []rune(strings.ToUpper(string(r)))[0])
				break
			}
		}
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	}
	return "application/octet-stream"
}
