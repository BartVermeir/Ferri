package middleware

import (
	"log/slog"
	"net/http"
	"net/url"
)

// CSRFProtect blocks state-changing requests (POST/PUT/PATCH/DELETE) whose
// Origin — or, failing that, Referer — does not match the host of
// allowedBaseURL (server.base_url). Together with SameSite=Strict session
// cookies this defends the admin panel and the internal send/request
// endpoints against cross-site request forgery: a form auto-submitted from an
// attacker page carries the attacker's Origin, which will not match, and is
// rejected. Without a base URL the request's Host is the reference.
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
					"method", r.Method, "route", firstSegment(r.URL.Path),
					"origin", r.Header.Get("Origin"), "referer_host", hostOf(r.Header.Get("Referer")))
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sameOrigin reports whether the request's Origin (or Referer) host matches
// allowedHost, or the request Host when there is none. A request carrying
// neither header is treated as unsafe and rejected.
func sameOrigin(r *http.Request, allowedHost string) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	host := hostOf(source)
	if host == "" {
		return false
	}
	if allowedHost == "" {
		return host == r.Host
	}
	return host == allowedHost
}

// hostOf returns the host of a URL, "" when there is none.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
