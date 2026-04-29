// Package installer is the in-app, end-to-end install/uninstall path
// for em-wall. The Wails UI runs unprivileged, so any operation that
// needs root (writing /Library/LaunchDaemons/, patching /etc/pf.conf,
// copying the daemon binary) is wrapped in osascript's "with
// administrator privileges" — macOS shows the standard authorization
// prompt and runs the bash payload as root.
//
// The daemon binary, plist, and pf anchor stub are embedded into the
// app binary via embed.FS. The Makefile target `app-bundle` populates
// resources/ with a freshly built em-walld before invoking
// `wails build`, so a single shipped .app is fully self-contained.
//
// During `wails dev` the resources/ directory may be empty — the
// installer detects that and surfaces ErrNotPackaged. Devs are
// expected to run the daemon directly via `make run-daemon`.
package installer

import "embed"

// resources holds the files we lay down at install time. Patterns
// pointing at a directory tolerate an empty dir, so `wails dev` builds
// even when the Makefile pre-build hasn't run.
//
//go:embed all:resources
var resources embed.FS
