// Package web hosts the Vue frontend sources (spec §10, §14 step 12)
// built at release time.
//
// The built assets live in web/dist and are embedded into the single
// static binary with go:embed, so end users never need Node installed
// (`make web` or CI builds dist from ../web/src; go:embed only reads
// whatever is already there). `ballast serve` mounts Handler at "/"
// alongside the REST API the same UI talks to via /api/v1.
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
