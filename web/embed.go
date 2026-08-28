// Package web embeds the built frontend into the binary (DESIGN.md §2).
// The dist directory is produced by `npm run build` (see Makefile) and is
// committed, so `go build` always works without a Node toolchain.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend rooted at the dist directory.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
