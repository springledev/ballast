// Package web hosts the Vue frontend sources built at release time.
//
// The built assets live in web/dist and are embedded into the single
// static binary with go:embed, so end users never need Node installed.
// At this scaffolding step (§14 step 1) dist contains a stub page that
// the `ballast serve` command exposes; later steps replace it with the
// real UI and the REST API it talks to.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var assets embed.FS

// FS returns the embedded UI assets rooted at web/dist.
func FS() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}

// Handler returns an http.Handler serving the embedded UI.
func Handler() (http.Handler, error) {
	sub, err := FS()
	if err != nil {
		return nil, err
	}
	return http.StripPrefix("/", http.FileServer(http.FS(sub))), nil
}
