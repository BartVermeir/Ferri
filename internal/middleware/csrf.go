package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// CSRFOriginCheck rejects state-changing requests whose Origin header (or,
// when absent, Referer) doesn't match the configured base URL's origin.
//
// These routes are IP-allowlisted but have no session cookie to attach a
// synchronizer/double-submit CSRF token to, and IP-allowlisting is not a
// CSRF defense on its own — a browser on the allowed network will still
// send a cross-site POST triggered by an external page. Origin verification
// is the standard mitigation for exactly this case, and both fetch() and
// plain HTML form submissions send Origin on POST requests in all current
// browsers.
func CSRFOriginCheck(baseURL string) func(http.Handler) http.Handler {
	expected := originOf(baseURL)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = originOf(r.Header.Get("Referer"))
			}
			if originOf(origin) != expected {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}
