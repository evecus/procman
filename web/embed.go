package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embeddedFS embed.FS

// StaticFS is the sub-filesystem rooted at web/static/
var StaticFS, _ = fs.Sub(embeddedFS, "static")
