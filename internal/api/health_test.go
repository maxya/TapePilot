package api

import (
	"net/http/httptest"
	"testing"
)

func TestRoutes(t *testing.T) {
	for _, x := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/health", 200}, {"POST", "/api/v1/health", 405}, {"GET", "/api/v1/jobs", 404}} {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest(x.method, x.path, nil))
		if w.Code != x.status {
			t.Fatal(w.Code)
		}
	}
}
