// Package web embeds the frontend so Cotta ships as a single binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var files embed.FS

// FS is the frontend, rooted at index.html.
var FS = func() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()
