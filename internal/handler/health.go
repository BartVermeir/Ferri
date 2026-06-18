package handler

import (
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/BartVermeir/Ferri/static"
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

// Static returns a handler that serves embedded static files from web/static/.
// Files are embedded at compile time via the static package — no runtime
// filesystem dependency. The binary is fully self-contained.
func Static() http.Handler {
	sub, err := fs.Sub(static.FS, "files")
	if err != nil {
		panic("static: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
