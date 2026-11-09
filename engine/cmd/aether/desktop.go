package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The Windows app, served by the server it connects to: /desktop/latest.json
// names the version the image shipped with, its file, hash and size, and the
// file is beside it. The app checks this to update itself; a person fetches
// the file to install. Only plain file names in the one directory are
// served, as downloads (never inline: bytes a browser could run are not
// served inline from this origin).
func desktopHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasSuffix(name, ".json") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		http.ServeFile(w, r, p)
	})
}
