// Package ui serves the console that ships inside the binary.
//
// The console used to be a second container whose Node server did two things:
// serve these files, and proxy /api to anamnesia with the server token
// attached so it never reached browser JavaScript. Served from the same
// process there is nothing to proxy and no token to attach, so both the
// proxy and the container are gone.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The Vite build writes here (see ui/vite.config.ts) and the result is
// committed, because go:embed needs it at compile time and `go install` has
// to keep working on a machine with no Node on it.
//
//go:embed all:dist
var bundle embed.FS

var dist = func() fs.FS {
	sub, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic("ui: embedded bundle has no dist/: " + err.Error())
	}
	return sub
}()

// Read once, so a binary built from an empty dist/ fails at startup rather
// than serving a blank page to someone who then has to guess why.
var index = func() []byte {
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		panic("ui: embedded bundle has no index.html; run `make ui-build`")
	}
	return b
}()

// Handler serves the console.
//
// A path that exists in the bundle is served as itself. Anything else returns
// index.html, because the console routes client-side and a reloaded deep link
// has to reach the app rather than a 404. apiPrefixes name the paths the API
// owns: an unmatched request under one of those is a typo'd endpoint, not a
// console route, and answering it with a web page reads as success to
// anything that is not a browser.
func Handler(apiPrefixes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		if info, err := fs.Stat(dist, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Asset names are content hashes, so a cached copy can never
				// be a stale version of a different file.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, dist, name)
			return
		}

		for _, prefix := range apiPrefixes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				http.Error(w, "No such endpoint.", http.StatusNotFound)
				return
			}
		}

		// index.html names the content-hashed assets, so a browser that
		// cached it would keep asking for a bundle this binary no longer has.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
