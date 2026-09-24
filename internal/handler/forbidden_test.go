package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
)

// The branded 403 page must keep the 403 status and must not echo the
// client's IP back.
func TestForbiddenPage(t *testing.T) {
	stores := newTestStores(t)
	h := appMiddleware.InjectSettings(stores.Settings)(Forbidden())

	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)

	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "only opens from the inside") {
		t.Fatal("forbidden page content missing")
	}
	if strings.Contains(body, "203.0.113.9") {
		t.Fatal("forbidden page leaks the client IP")
	}
}
