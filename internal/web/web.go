// SPDX-License-Identifier: GPL-3.0-or-later

// Package web holds the browser UI: static files embedded in the binary,
// served by the coordinator. Plain HTML, CSS and JavaScript with no build
// step; the pages talk to the same JSON API and SSE streams as the CLI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// FS is the UI's files, rooted at the assets directory.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
