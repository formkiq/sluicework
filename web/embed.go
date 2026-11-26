// Package web serves the compiled Vite application embedded in the executable.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var content embed.FS

func Handler() http.Handler {
	dist, err := fs.Sub(content, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", 405)
			return
		}
		// Client-side routes can also be opened as bookmarks.
		if r.URL.Path != "/" {
			if _, err := fs.Stat(dist, r.URL.Path[1:]); err != nil && r.Header.Get("Sec-Fetch-Mode") == "navigate" {
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}
