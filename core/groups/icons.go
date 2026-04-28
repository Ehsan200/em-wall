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
