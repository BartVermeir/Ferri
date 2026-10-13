package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/db"
	"github.com/BartVermeir/Ferri/internal/jobs"
	"github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
	ferritls "github.com/BartVermeir/Ferri/internal/tus"
)

// These tests use the router that main serves.

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	return testRouterWith(t, func(*config.Config) {})
}

// testRouterWith builds the router after mutate has changed the config.
func testRouterWith(t *testing.T, mutate func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.BaseURL = "http://ferri.test"
	cfg.Server.Location = time.UTC
	cfg.Admin.Token = strings.Repeat("x", 32)
	cfg.Storage.Path = t.TempDir()
	_, internal, _ := net.ParseCIDR("10.0.0.0/8")
	cfg.IPAllowlist = []*net.IPNet{internal}
	mutate(cfg)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	stores := store.New(database)
	mgr := storage.NewManager(storage.NewLocalBackend(cfg.Storage.Path))
	tusHandler, err := ferritls.NewHandler(cfg, stores, mgr)
	if err != nil {
		t.Fatal(err)
	}
	return newRouter(cfg, database, stores, mgr, tusHandler, jobs.NewScheduler(cfg, stores, mgr))
}

// The full route list; it must match the routes main registers.
func TestRouter_Routes(t *testing.T) {
	want := []string{
		"GET /", "POST /send", "GET /request", "POST /request",
		"POST /login", "GET /login/{token}", "POST /login/{token}", "POST /logout",
		"GET /manage/{token}", "POST /manage/{token}/extend", "POST /manage/{token}/recipients", "POST /manage/{token}/delete",
		"GET /health", "GET /favicon.ico", "* /static/*", "* /static/logo/*", "* /tus/*",
		"GET /dl/{token}", "POST /dl/{token}", "GET /dl/{token}/file/{fileID}", "GET /dl/{token}/zip",
		"GET /ul/{token}", "POST /ul/{token}", "POST /ul/{token}/complete",
		"GET /ul/{token}/files", "POST /ul/{token}/files", "GET /ul/{token}/file/{fileID}", "GET /ul/{token}/zip",
		"GET /admin/login", "POST /admin/login", "GET /admin", "POST /admin/cleanup", "POST /admin/orphans/clean",
		"GET /admin/transfers", "GET /admin/transfers/{id}/files", "GET /admin/transfers/{id}/file/{fileID}",
		"GET /admin/transfers/{id}/stats", "GET /admin/requests/{id}/stats",
		"GET /admin/requests/{id}/files", "GET /admin/requests/{id}/file/{fileID}",
		"POST /admin/transfers/{id}/delete", "POST /admin/requests/{id}/delete", "POST /admin/delete",
		"GET /admin/history", "GET /admin/diag", "GET /admin/diag/{tusID}", "GET /admin/mail", "POST /admin/mail/{id}/retry", "POST /admin/mail/{id}/delete",
		"GET /admin/settings", "GET /admin/settings/branding", "POST /admin/settings/branding",
		"GET /admin/settings/mail", "POST /admin/settings/mail", "GET /admin/settings/storage", "POST /admin/settings/logo", "POST /admin/settings/logo/delete",
		"POST /admin/settings/storage", "POST /admin/settings/storage/test", "POST /admin/logout",
	}
	got := map[string]bool{}
	err := chi.Walk(testRouter(t).(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasSuffix(route, "/*") {
			method = "*" // r.Handle and r.Mount register every method
		}
		got[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, extra []string
	for _, w := range want {
		if !got[w] {
			missing = append(missing, w)
		}
		delete(got, w)
	}
	for g := range got {
		extra = append(extra, g)
	}
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("routes differ from the list.\nmissing: %q\nnot in the list: %q", missing, extra)
	}
}

func TestRouter_AccessRules(t *testing.T) {
	r := testRouter(t)
	do := func(method, path, from string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = from + ":4000"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
	const outside, inside = "203.0.113.9", "10.1.2.3"

	cases := []struct {
		name, method, path, from string
		hdr                      map[string]string
		want                     int
	}{
		{"health is public", "GET", "/health", outside, nil, http.StatusOK},
		{"send page from outside", "GET", "/", outside, nil, http.StatusForbidden},
		{"send page from inside", "GET", "/", inside, nil, http.StatusOK},
		{"admin from outside", "GET", "/admin", outside, nil, http.StatusForbidden},
		{"admin without session", "GET", "/admin", inside, nil, http.StatusSeeOther},
		{"manage page from outside", "GET", "/manage/nope", outside, nil, http.StatusForbidden},
		{"manage delete from outside", "POST", "/manage/nope/delete", outside, nil, http.StatusForbidden},
		{"unknown manage link from inside", "GET", "/manage/nope", inside, nil, http.StatusNotFound},
		{"unknown download link", "GET", "/dl/nope", outside, nil, http.StatusNotFound},
		{"requester password route exists", "POST", "/ul/nope/files", outside, nil, http.StatusNotFound},
		{"TUS reaches the pre-create hook", "POST", "/tus/", outside, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if rr := do(c.method, c.path, c.from, c.hdr); rr.Code != c.want {
			t.Errorf("%s: %s %s from %s = %d, want %d", c.name, c.method, c.path, c.from, rr.Code, c.want)
		}
	}
}

// A signed-in colleague outside the internal network reaches the send and
// request pages, not the manage pages or the admin.
func TestRouter_RemoteSignIn(t *testing.T) {
	var cfg *config.Config
	r := testRouterWith(t, func(c *config.Config) {
		c.RemoteLogin.Domains = []string{"example.com"}
		cfg = c
	})
	rr := httptest.NewRecorder()
	middleware.SetRemoteCookie(rr, cfg, "alice@example.com")
	cookie := rr.Result().Cookies()[0]

	const outside = "203.0.113.9"
	do := func(method, path string, signedIn bool) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = outside + ":4000"
		req.Header.Set("Origin", "http://ferri.test")
		if signedIn {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code
	}
	cases := []struct {
		method, path string
		signedIn     bool
		want         int
	}{
		{"GET", "/", false, http.StatusForbidden},
		{"GET", "/", true, http.StatusOK},
		{"GET", "/request", true, http.StatusOK},
		{"GET", "/manage/nope", true, http.StatusForbidden},
		{"GET", "/admin", true, http.StatusForbidden},
		{"GET", "/admin/login", true, http.StatusForbidden},
		{"GET", "/login/nope", false, http.StatusNotFound},
		{"POST", "/logout", true, http.StatusSeeOther},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.signedIn); got != c.want {
			t.Errorf("%s %s (signed in %v) = %d, want %d", c.method, c.path, c.signedIn, got, c.want)
		}
	}
}

// Without remote_login.domains, the sign-in routes do not exist for anyone.
func TestRouter_RemoteSignInOff(t *testing.T) {
	r := testRouter(t)
	for _, path := range []string{"/login", "/logout"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("email=alice@example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://ferri.test")
		req.RemoteAddr = "203.0.113.9:4000"
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, rr.Code)
		}
	}
}
