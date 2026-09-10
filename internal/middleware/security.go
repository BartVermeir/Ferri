package middleware

import "net/http"

// contentSecurityPolicy still allows 'unsafe-inline' for scripts and styles
// because every page ships inline <style> and a few small inline <script> blocks.
// The TUS client is now vendored (static/files/tus.min.js) so no external script
// host is permitted at all — script-src is 'self' only. object/base/frame-ancestors
// are locked down. Next step: move the remaining inline <script> blocks to files
// (or add per-request nonces) and drop 'unsafe-inline' from script-src.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
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
