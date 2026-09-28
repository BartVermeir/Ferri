package middleware

import (
	"log/slog"
	"net/http"
	"net/url"
)

// CSRFProtect blocks state-changing requests (POST/PUT/PATCH/DELETE) whose
// Origin — or, failing that, Referer — does not match the host the request was
// sent to. Together with SameSite=Strict session cookies this defends the admin
// panel and the internal send/request endpoints against cross-site request
// forgery: a form auto-submitted from an attacker page carries the attacker's
// Origin, which will not match, and is rejected.
//
// allowedBaseURL (server.base_url) is accepted as an additional valid origin so
// deployments behind a reverse proxy that rewrites the Host header keep working.
// Safe methods (GET/HEAD/OPTIONS/TRACE) always pass through.
func CSRFProtect(allowedBaseURL string) func(http.Handler) http.Handler {
	allowedHost := ""
	if u, err := url.Parse(allowedBaseURL); err == nil {
		allowedHost = u.Host
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
				next.ServeHTTP(w, r)
				return
			}
			if !sameOrigin(r, allowedHost) {
				slog.Warn("csrf: cross-origin request blocked",
					"method", r.Method, "path", r.URL.Path,
					"origin", r.Header.Get("Origin"), "referer", r.Header.Get("Referer"))
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sameOrigin reports whether the request's Origin (or Referer) host matches the
// request Host or the configured allowed host. A request carrying neither header
// is treated as unsafe and rejected.
func sameOrigin(r *http.Request, allowedHost string) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return false
	}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host || (allowedHost != "" && u.Host == allowedHost)
}
