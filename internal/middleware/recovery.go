package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery returns middleware that catches panics, logs them with the stack trace,
// and returns HTTP 500 to the client.
func Recovery() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// Deliberate abort of a response already under way (a ZIP
					// that failed mid-stream): let net/http cut the connection
					// so the client sees a failed download, not a 500 page.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					slog.Error("panic recovered",
						"panic", rec,
						"stack", string(debug.Stack()),
						"method", r.Method,
						"path", r.URL.Path,
					)
					http.Error(w, "Internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
