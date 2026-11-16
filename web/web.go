// Package web embeds the HTML templates and static assets into the binary so
// the application ships as a single executable.
package web

import (
	"embed"
	"io/fs"
)

//go:embed templates static
var files embed.FS

// Templates returns the template tree rooted at web/templates.
func Templates() fs.FS { return sub("templates") }

// Static returns the static asset tree rooted at web/static.
func Static() fs.FS { return sub("static") }

func sub(dir string) fs.FS {
	f, err := fs.Sub(files, dir)
	if err != nil {
		// Only possible if the embed directive and dir disagree: a programming error.
		panic(err)
	}
	return f
}
