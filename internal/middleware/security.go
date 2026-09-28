package middleware

import "net/http"

// contentSecurityPolicy allows 'unsafe-inline' for styles only — every page
// ships inline <style> blocks. All <script> tags are now external (the TUS
// client is vendored in static/files/tus.min.js, and the handful of former
// inline scripts moved to static/files/*.js), so script-src is 'self' with
// no 'unsafe-inline' and no external script host at all. object/base/
// frame-ancestors are locked down too.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// SecurityHeaders adds standard defensive HTTP headers to every response.
// secure should match server.secure_cookies (i.e. the app is served over HTTPS);
// when true, HSTS is emitted.
func SecurityHeaders(secure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
			if secure {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
