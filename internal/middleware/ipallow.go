package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// IPAllow returns middleware that restricts access to requests from the given CIDR ranges.
// If allowlist is empty, all requests are blocked (fail-safe).
// Respects X-Forwarded-For set by a trusted reverse proxy.
func IPAllow(allowlist []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			parsed := net.ParseIP(ip)
			if parsed == nil {
				slog.Warn("ip allowlist: could not parse client IP", "raw", ip)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			for _, network := range allowlist {
				if network.Contains(parsed) {
					next.ServeHTTP(w, r)
					return
				}
			}

			slog.Warn("ip allowlist: blocked", "ip", ip, "path", r.URL.Path)
			http.Error(w, "Forbidden", http.StatusForbidden)
		})
	}
}

// clientIP extracts the real client IP, preferring X-Real-IP then X-Forwarded-For,
// then falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return strings.TrimSpace(ip)
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		// Take the first (leftmost) IP — the original client
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
