package middleware

import (
	"net/http"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		origin      string
		referer     string
		allowedHost string
		want        bool
	}{
		{name: "origin matches host", host: "send.example.com", origin: "https://send.example.com", want: true},
		{name: "origin mismatch", host: "send.example.com", origin: "https://evil.example", want: false},
		{name: "no origin, referer matches host", host: "send.example.com", referer: "https://send.example.com/admin", want: true},
		{name: "no origin, referer mismatch", host: "send.example.com", referer: "https://evil.example/x", want: false},
		{name: "neither header present", host: "send.example.com", want: false},
		{name: "origin matches configured base_url host", host: "app:8080", origin: "https://send.example.com", allowedHost: "send.example.com", want: true},
		{name: "origin empty string", host: "send.example.com", origin: "", referer: "", want: false},
		{name: "origin null", host: "send.example.com", origin: "null", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "/send", nil)
			r.Host = tt.host
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				r.Header.Set("Referer", tt.referer)
			}
			if got := sameOrigin(r, tt.allowedHost); got != tt.want {
				t.Fatalf("sameOrigin = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCSRFProtectAllowsSafeMethods(t *testing.T) {
	h := CSRFProtect("https://send.example.com")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r, _ := http.NewRequest(m, "/", nil) // no Origin/Referer
		rw := &statusRecorder{code: 0}
		h.ServeHTTP(rw, r)
		if rw.code != http.StatusOK {
			t.Fatalf("%s without Origin header should pass, got %d", m, rw.code)
		}
	}
}

func TestCSRFProtectBlocksCrossOriginPost(t *testing.T) {
	h := CSRFProtect("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r, _ := http.NewRequest(http.MethodPost, "/send", nil)
	r.Host = "send.example.com"
	r.Header.Set("Origin", "https://evil.example")
	rw := &statusRecorder{code: 0}
	h.ServeHTTP(rw, r)
	if rw.code != http.StatusForbidden {
		t.Fatalf("cross-origin POST should be 403, got %d", rw.code)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) Header() http.Header        { return http.Header{} }
func (s *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusRecorder) WriteHeader(code int)        { s.code = code }
