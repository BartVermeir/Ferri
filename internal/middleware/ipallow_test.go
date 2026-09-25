package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return n
}

func TestClientIP(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}

	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "direct client, no headers",
			remoteAddr: "203.0.113.9:5555",
			want:       "203.0.113.9",
		},
		{
			name:       "untrusted client cannot spoof X-Real-IP",
			remoteAddr: "203.0.113.9:5555",
			headers:    map[string]string{"X-Real-IP": "10.1.2.3"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted proxy sets X-Real-IP",
			remoteAddr: "10.9.9.9:443",
			headers:    map[string]string{"X-Real-IP": "198.51.100.7"},
			want:       "198.51.100.7",
		},
		{
			name:       "trusted proxy X-Forwarded-For takes first hop",
			remoteAddr: "10.9.9.9:443",
			headers:    map[string]string{"X-Forwarded-For": "198.51.100.7, 10.9.9.9"},
			want:       "198.51.100.7",
		},
		{
			name:       "trusted proxy, no forwarding headers",
			remoteAddr: "10.9.9.9:443",
			want:       "10.9.9.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := ClientIP(r, trusted); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPNoTrustedProxies(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.9.9.9:443"
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := ClientIP(r, nil); got != "10.9.9.9" {
		t.Fatalf("with no trusted proxies, headers must be ignored; got %q", got)
	}
}

func TestIPAllowBlockedUsesDeniedHandler(t *testing.T) {
	allow := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	denied := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := IPAllow(allow, nil, denied)(next)

	tests := []struct {
		remoteAddr string
		want       int
	}{
		{"10.1.2.3:1234", http.StatusOK},
		{"203.0.113.9:1234", http.StatusTeapot},
		{"garbage", http.StatusTeapot},
	}
	for _, tt := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.remoteAddr
		rw := &statusRecorder{}
		h.ServeHTTP(rw, r)
		if rw.code != tt.want {
			t.Fatalf("%s: got %d, want %d", tt.remoteAddr, rw.code, tt.want)
		}
	}
}

// Audit L14: with only X-Forwarded-For, the visitor is the rightmost address
// that is not a trusted proxy; the leftmost one is whatever the client sent.
func TestClientIP_ForwardedForTakesRightmostUntrusted(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("10.200.0.1/32")
	_, lb, _ := net.ParseCIDR("10.9.0.0/16")
	trusted := []*net.IPNet{proxy, lb}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.200.0.1:5555"
	req.Header.Set("X-Forwarded-For", "192.168.1.10, 203.0.113.7, 10.9.1.1")
	if got := ClientIP(req, trusted); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want 203.0.113.7 (the spoofed 192.168.1.10 must not win)", got)
	}
}
