package middleware

import (
	"net/http"
	"strings"
)

// BodyLimit caps every request body at n bytes, except under /tus (uploads,
// limited by limits.max_upload_bytes). Reading past the cap fails, so a form
// parse returns an error.
func BodyLimit(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/tus" && !strings.HasPrefix(r.URL.Path, "/tus/") {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
