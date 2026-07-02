package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is a simple in-memory fixed-window limiter, intended as
// brute-force protection on authentication endpoints (admin login and
// password-protected download/upload pages) — not as a general throttle.
//
// The key combines the trusted-proxy-aware client IP with the request path, so
// password endpoints (whose path contains the transfer/request token) are
// limited per (IP, token): a shared NAT egress IP does not cause one visitor's
// attempts against transfer A to lock out another visitor's access to transfer B.
// For the admin login the path is constant, so it limits per IP.
//
// State is per-process, which is sufficient for Ferri's single-container
// deployment. It is not shared across replicas.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*rlWindow
	max      int
	interval time.Duration
	proxies  []*net.IPNet
	now      func() time.Time
}

type rlWindow struct {
	count int
	reset time.Time
}

// NewRateLimiter creates a limiter allowing max requests per interval per key.
func NewRateLimiter(max int, interval time.Duration, trustedProxies []*net.IPNet) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*rlWindow),
		max:      max,
		interval: interval,
		proxies:  trustedProxies,
		now:      time.Now,
	}
}

// Middleware rejects requests exceeding the limit with HTTP 429 and a Retry-After
// header. Only the wrapped (state-changing) method should be limited — wire it
// with chi's r.With(limiter.Middleware).Post(...) so GET page renders are unaffected.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r, rl.proxies) + "|" + r.URL.Path
		if !rl.allow(key) {
			slog.Warn("rate limit: request throttled", "path", r.URL.Path)
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.interval.Seconds())))
			http.Error(w, "Too many attempts. Please wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	win, ok := rl.windows[key]
	if !ok || now.After(win.reset) {
		rl.windows[key] = &rlWindow{count: 1, reset: now.Add(rl.interval)}
		// Opportunistic cleanup so the map cannot grow unbounded under attack.
		if len(rl.windows) > 4096 {
			for k, v := range rl.windows {
				if now.After(v.reset) {
					delete(rl.windows, k)
				}
			}
		}
		return true
	}
	if win.count >= rl.max {
		return false
	}
	win.count++
	return true
}
