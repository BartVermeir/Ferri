package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersCSP(t *testing.T) {
	h := SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header set")
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("script-src is not 'self'-based: %q", csp)
	}
	// The TUS client is vendored — no external script host may be allowed.
	for _, host := range []string{"jsdelivr", "unpkg", "https://cdn", "http://"} {
		if strings.Contains(csp, host) {
			t.Errorf("CSP still references external host %q: %q", host, csp)
		}
	}
	for _, want := range []string{"object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q", want)
		}
	}
}

func TestSecurityHeadersHSTSOnlyWhenSecure(t *testing.T) {
	insecure := httptest.NewRecorder()
	SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(insecure, httptest.NewRequest("GET", "/", nil))
	if insecure.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS set when secure_cookies is false")
	}

	secure := httptest.NewRecorder()
	SecurityHeaders(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(secure, httptest.NewRequest("GET", "/", nil))
	if secure.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS not set when secure_cookies is true")
	}
}
