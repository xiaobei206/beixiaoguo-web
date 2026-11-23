package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html assets/*
var embedded embed.FS

// Handler returns the embedded, same-origin web application.
func Handler() http.Handler {
	root, err := fs.Sub(embedded, ".")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(root))
}
