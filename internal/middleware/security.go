package middleware

import "net/http"

// contentSecurityPolicy is intentionally permissive on inline styles/scripts
// because every page ships inline <style> and <script> blocks, and the public
// upload/send pages load the TUS client from jsDelivr. It still meaningfully
// hardens the app: object/base/frame-ancestors are locked down and only that one
// external script host is allowed. Tighten to nonces + 'self' if the inline
// blocks and the CDN dependency are removed.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' https://cdn.jsdelivr.net 'unsafe-inline'; " +
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
