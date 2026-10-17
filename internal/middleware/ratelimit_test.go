package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func limiterAt(max int, now *time.Time) *RateLimiter {
	rl := NewRateLimiter(max, time.Minute, nil)
	rl.now = func() time.Time { return *now }
	return rl
}

func hit(rl *RateLimiter, remote, path string) int {
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = remote
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// Per IP and path: max requests per window, then 429 until the window ends.
func TestRateLimiter_PerPath(t *testing.T) {
	now := time.Now()
	rl := limiterAt(3, &now)
	for i := range 3 {
		if code := hit(rl, "192.0.2.1:1", "/dl/a"); code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, code)
		}
	}
	if code := hit(rl, "192.0.2.1:1", "/dl/a"); code != http.StatusTooManyRequests {
		t.Fatalf("4th request: %d, want 429", code)
	}
	if code := hit(rl, "192.0.2.2:1", "/dl/a"); code != http.StatusOK {
		t.Fatalf("other IP: %d", code)
	}
	now = now.Add(time.Minute + time.Second)
	if code := hit(rl, "192.0.2.1:1", "/dl/a"); code != http.StatusOK {
		t.Fatalf("after the window: %d", code)
	}
}

// One IP cannot get around the limit with a new path per request, and an
// IPv6 client counts per /64.
func TestRateLimiter_PerIPOverAllPaths(t *testing.T) {
	now := time.Now()
	rl := limiterAt(2, &now)
	ok := 0
	for i := range 50 {
		if hit(rl, "[2001:db8:1:2::"+strconv.Itoa(i+1)+"]:1", "/dl/"+strconv.Itoa(i)) == http.StatusOK {
			ok++
		}
	}
	if ok != 2*perIPFactor {
		t.Fatalf("%d requests allowed from one /64, want %d", ok, 2*perIPFactor)
	}
}

// The number of windows is bounded, and so is the path in a key.
func TestRateLimiter_Bounded(t *testing.T) {
	now := time.Now()
	rl := limiterAt(10, &now)
	long := "/dl/" + strings.Repeat("x", 8000)
	for i := range rlMaxKeys {
		hit(rl, "198.51."+strconv.Itoa(i/250)+"."+strconv.Itoa(i%250)+":1", long)
	}
	if n := len(rl.windows); n > rlMaxKeys {
		t.Fatalf("%d windows, want at most %d", n, rlMaxKeys)
	}
	for k := range rl.windows {
		if len(k) > 64+1+rlMaxPathKey {
			t.Fatalf("key of %d bytes", len(k))
		}
	}
	if code := hit(rl, "203.0.113.9:1", "/dl/new"); code != http.StatusTooManyRequests {
		t.Fatalf("new client with the limiter full: %d, want 429", code)
	}
	now = now.Add(2 * time.Minute)
	if code := hit(rl, "203.0.113.9:1", "/dl/new"); code != http.StatusOK {
		t.Fatalf("after the windows expired: %d", code)
	}
}

func TestFirstSegment(t *testing.T) {
	for in, want := range map[string]string{"/dl/abc/file/x": "/dl", "/admin/login": "/admin", "/login": "/login", "/": "/"} {
		if got := firstSegment(in); got != want {
			t.Errorf("firstSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
