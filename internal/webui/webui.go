// Package webui embeds the built web UI into the binary so a freshly
// compiled local-codex serves the UI with no extra steps.
//
// `make build` copies the real frontend build (web/dist) into dist/ before
// compiling; only the placeholder .gitkeep is committed, so `go build` and
// `go test` always compile even when the frontend was never built.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded UI files rooted at dist/, and whether a real
// frontend build is present. ok is false when only the placeholder was
// embedded; callers should then fall back to serving web/dist from disk.
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	_, err = fs.Stat(sub, "index.html")
	return sub, err == nil
}
