package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a simple in-memory fixed-window limiter, intended as
// brute-force protection on authentication endpoints (admin login and
// password-protected download/upload pages) — not as a general throttle.
//
// Two windows count each request: one per client IP and path, so password
// endpoints (whose path contains the transfer/request token) are limited per
// (IP, token), and one per client IP over all paths (perIPFactor times the
// limit). IPv6 clients count per /64. For the admin login the path is
// constant, so it limits per IP.
//
// State is per-process, not shared across replicas.
type RateLimiter struct {
	mu        sync.Mutex
	windows   map[string]*rlWindow
	max       int
	perIP     int
	interval  time.Duration
	proxies   []*net.IPNet
	now       func() time.Time
	lastSweep time.Time
}

type rlWindow struct {
	count int
	reset time.Time
}

const (
	// perIPFactor: requests per IP over all paths, as a multiple of the
	// per-path limit.
	perIPFactor = 5
	// rlMaxKeys is the most windows kept. When all of them are live, a
	// request that needs a new one is refused.
	rlMaxKeys = 20000
	// rlSweepAt: from this many windows, expired ones are removed when a new
	// one is made, at most once a second.
	rlSweepAt = 4096
	// rlMaxPathKey is the most bytes of the path used in a key.
	rlMaxPathKey = 128
)

// NewRateLimiter creates a limiter allowing max requests per interval per IP
// and path, and perIPFactor times that per IP.
func NewRateLimiter(max int, interval time.Duration, trustedProxies []*net.IPNet) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*rlWindow),
		max:      max,
		perIP:    max * perIPFactor,
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
		ipKey := clientBucket(ClientIP(r, rl.proxies))
		p := r.URL.Path
		if len(p) > rlMaxPathKey {
			p = p[:rlMaxPathKey]
		}
		if !rl.allow(ipKey, ipKey+"|"+p) {
			slog.Warn("rate limit: request throttled", "route", firstSegment(r.URL.Path))
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.interval.Seconds())))
			http.Error(w, "Too many attempts. Please wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow counts a request in the IP window and the IP+path window; it is
// refused when either is full, or when no window can be made.
func (rl *RateLimiter) allow(ipKey, pathKey string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	ipWin, ok := rl.window(ipKey, now)
	if !ok {
		return false
	}
	pathWin, ok := rl.window(pathKey, now)
	if !ok {
		return false
	}
	if ipWin.count >= rl.perIP || pathWin.count >= rl.max {
		return false
	}
	ipWin.count++
	pathWin.count++
	return true
}

// window returns the live window for key, starting a new one when there is
// none or it expired. False when the limiter holds rlMaxKeys live windows.
// Must be called with rl.mu held.
func (rl *RateLimiter) window(key string, now time.Time) (*rlWindow, bool) {
	w, exists := rl.windows[key]
	if exists && !now.After(w.reset) {
		return w, true
	}
	if !exists {
		if len(rl.windows) >= rlSweepAt {
			rl.sweep(now)
		}
		if len(rl.windows) >= rlMaxKeys {
			return nil, false
		}
	}
	w = &rlWindow{reset: now.Add(rl.interval)}
	rl.windows[key] = w
	return w, true
}

// sweep removes expired windows, at most once a second.
func (rl *RateLimiter) sweep(now time.Time) {
	if now.Sub(rl.lastSweep) < time.Second {
		return
	}
	rl.lastSweep = now
	for k, v := range rl.windows {
		if now.After(v.reset) {
			delete(rl.windows, k)
		}
	}
}

// clientBucket is the client's IP, or its /64 network for IPv6.
func clientBucket(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// firstSegment returns the first segment of a path ("/dl/abc" → "/dl"), so a
// log line does not carry the token after it.
func firstSegment(p string) string {
	if i := strings.IndexByte(strings.TrimPrefix(p, "/"), '/'); i >= 0 {
		return p[:i+1]
	}
	return p
}
