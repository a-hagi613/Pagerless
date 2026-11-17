// Package dashboard embeds the built React dashboard so that pagerless-server serves it from its own binary
// (ADR-0013). It holds no application code: the views and their build live beside this file, and the Vite build
// writes them into dist, which the server embeds at compile time.
package dashboard

import (
	"embed"
	"io/fs"
)

// assets contains the Vite build output. docker build runs the build before go build; a local binary embeds
// whatever dist holds, and the repository commits only the directory marker.
//
//go:embed all:dist
var assets embed.FS

// Assets returns the embedded dashboard files, rooted at the directory that holds index.html.
func Assets() fs.FS {
	return assets
}
