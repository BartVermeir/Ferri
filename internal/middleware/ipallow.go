package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

// untrustedForwardWarn makes sure the misconfiguration warning in ClientIP is
// logged once per process, not on every request.
var untrustedForwardWarn sync.Once

// IPAllow returns middleware that restricts access to requests from the given CIDR ranges.
// If allowlist is empty, all requests are blocked (fail-safe).
// Blocked requests are passed to denied, which must respond with a 403.
//
// Forwarded headers (X-Real-IP, X-Forwarded-For) are only trusted when the direct
// TCP connection originates from a configured trusted proxy. This prevents clients
// from spoofing their IP by injecting these headers directly.
func IPAllow(allowlist []*net.IPNet, trustedProxies []*net.IPNet, denied http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r, trustedProxies)
			parsed := net.ParseIP(ip)
			if parsed == nil {
				slog.Warn("ip allowlist: could not parse client IP", "raw", ip)
				denied.ServeHTTP(w, r)
				return
			}

			for _, network := range allowlist {
				if network.Contains(parsed) {
					next.ServeHTTP(w, r)
					return
				}
			}

			slog.Warn("ip allowlist: blocked", "ip", ip, "path", r.URL.Path)
			denied.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the real client IP using the same trusted-proxy logic as the
// IP allowlist. Use this anywhere a client IP is recorded (e.g. audit logs) so a
// direct client cannot spoof it via X-Real-IP / X-Forwarded-For headers.
// It only trusts X-Real-IP / X-Forwarded-For when the direct TCP connection
// (r.RemoteAddr) comes from a known trusted proxy. Otherwise RemoteAddr is used.
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
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

	// Forwarding headers from a peer we don't trust almost always means the
	// reverse proxy is missing from (or mistyped in) server.trusted_proxies.
	// The request is then judged on the proxy's IP, not the visitor's.
	if r.Header.Get("X-Real-IP") != "" || r.Header.Get("X-Forwarded-For") != "" {
		untrustedForwardWarn.Do(func() {
			slog.Warn("forwarding headers received from an untrusted peer — if this is your reverse proxy, add it to server.trusted_proxies",
				"peer", remoteHost)
		})
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
