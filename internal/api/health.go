package api

import "net/http"

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("{\"status\":\"ok\",\"mode\":\"simulator\"}\n"))
	})
	return mux
}
