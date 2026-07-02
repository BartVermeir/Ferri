package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// IPAllow returns middleware that restricts access to requests from the given CIDR ranges.
// If allowlist is empty, all requests are blocked (fail-safe).
//
// Forwarded headers (X-Real-IP, X-Forwarded-For) are only trusted when the direct
// TCP connection originates from a configured trusted proxy. This prevents clients
// from spoofing their IP by injecting these headers directly.
func IPAllow(allowlist []*net.IPNet, trustedProxies []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r, trustedProxies)
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

// ClientIP returns the real client IP using the same trusted-proxy logic as the
// IP allowlist. Use this anywhere a client IP is recorded (e.g. audit logs) so a
// direct client cannot spoof it via X-Real-IP / X-Forwarded-For headers.
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	return clientIP(r, trustedProxies)
}

// clientIP returns the real client IP.
// It only trusts X-Real-IP / X-Forwarded-For when the direct TCP connection
// (r.RemoteAddr) comes from a known trusted proxy. Otherwise RemoteAddr is used.
func clientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	remoteIP := net.ParseIP(remoteHost)
	if remoteIP != nil && containsIP(remoteIP, trustedProxies) {
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			return strings.TrimSpace(ip)
		}
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			return strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
		}
	}

	return remoteHost
}

func containsIP(ip net.IP, networks []*net.IPNet) bool {
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
