// Package webui serves a static console embedded only in native release builds.
package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

func staticHandler(assets fs.FS, api http.Handler) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'; connect-src 'self'")
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "."
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		info, err := fs.Stat(assets, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if info.IsDir() {
			if _, err = fs.Stat(assets, path.Join(name, "index.html")); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/_app/immutable/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
