package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html manifest.webmanifest assets/*
var embedded embed.FS

// Handler returns the embedded, same-origin web application.
func Handler() http.Handler {
	root, err := fs.Sub(embedded, ".")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
