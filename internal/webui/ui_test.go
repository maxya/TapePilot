package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticAssetsPreserveAPIBoundary(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("TapePilot synthetic static shell")}, "_app/immutable/start.js": {Data: []byte("export default true;")}}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"auth required"}`))
	})
	handler := staticHandler(assets, api)
	for _, p := range []string{"/api", "/api/v1/jobs", "/api/unknown"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 401 || w.Header().Get("Content-Type") != "application/json" || strings.Contains(w.Body.String(), "shell") {
			t.Fatal(p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/_app/", "/missing", "/../outside"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 404 {
			t.Fatal(p, w.Code)
		}
	}
	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest("GET", "/", nil))
	if root.Code != 200 || !strings.Contains(root.Body.String(), "TapePilot") || root.Header().Get("X-Frame-Options") != "DENY" || root.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal(root.Code, root.Header())
	}
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest("GET", "/_app/immutable/start.js", nil))
	if asset.Code != 200 || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatal(asset.Code, asset.Header())
	}
}
func TestWrapUsesSelectedBuildMode(t *testing.T) {
	handler := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if Embedded() {
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	} else if w.Code != 418 {
		t.Fatal(w.Code)
	}
}
