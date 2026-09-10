package middleware

import (
	"net"
	"net/http"
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
			if got := clientIP(r, trusted); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPNoTrustedProxies(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.9.9.9:443"
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := clientIP(r, nil); got != "10.9.9.9" {
		t.Fatalf("with no trusted proxies, headers must be ignored; got %q", got)
	}
}
