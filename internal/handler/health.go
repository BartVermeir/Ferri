package handler

import (
	"encoding/json"
	"net/http"
)

// Health returns a handler for GET /health.
// Returns HTTP 200 with {"status":"ok"}.
// Used by Docker healthcheck and external monitoring.
func Health() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// Static returns a handler that serves embedded static files.
func Static() http.Handler {
	// TODO: return http.FileServer(http.FS(staticFS)) with go:embed
	return http.NotFoundHandler()
}
