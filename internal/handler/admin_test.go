package handler

// HTTP-level tests for the admin auth gate: routes under /admin require a
// valid signed session cookie; POST routes also go through CSRFProtect,
// mirroring the middleware stack wired in cmd/server/main.go.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

// newAdminRouter mirrors the admin route group in cmd/server/main.go: login
// routes are CSRF-protected but open, everything else additionally requires
// AdminAuth. IP allowlisting is intentionally left out — it is covered by
// internal/middleware/ipallow_test.go and is orthogonal to the session gate
// under test here.
func newAdminRouter(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.Handler {
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Use(appMiddleware.CSRFProtect(cfg.Server.BaseURL))
	r.Get("/admin/login", AdminLogin(cfg))
	r.Post("/admin/login", AdminLoginPost(cfg))
	r.Group(func(r chi.Router) {
		r.Use(appMiddleware.AdminAuth(cfg))
		r.Get("/admin", AdminDashboard(cfg, stores))
		r.Post("/admin/transfers/{id}/delete", AdminTransferDelete(cfg, stores, mgr))
		r.Post("/admin/logout", AdminLogout(cfg))
	})
	return r
}

// withOrigin sets the Origin header CSRFProtect requires on state-changing
// requests so tests exercise the AdminAuth gate itself, not the CSRF check.
func withOrigin(req *http.Request, cfg *config.Config) *http.Request {
	req.Header.Set("Origin", cfg.Server.BaseURL)
	return req
}

func adminSessionCookie(t *testing.T, cfg *config.Config) *http.Cookie {
	t.Helper()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))
	form := url.Values{"token": {cfg.Admin.Token}}
	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode())), cfg)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login did not set a session cookie")
	}
	return cookies[0]
}

func mustManager(t *testing.T) *storage.Manager {
	t.Helper()
	mgr, _ := newTestManager(t)
	return mgr
}

func TestAdminDashboard_NoCookieRedirectsToLogin(t *testing.T) {
	cfg := newTestConfig()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to login", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q, want /admin/login", loc)
	}
}

func TestAdminDashboard_InvalidCookieRedirectsToLogin(t *testing.T) {
	cfg := newTestConfig()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: "ferri_admin", Value: "garbage"})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to login", rr.Code)
	}
}

func TestAdminDashboard_ValidCookieGrantsAccess(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr := mustManager(t)
	r := newAdminRouter(cfg, stores, mgr)

	cookie := adminSessionCookie(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminLoginPost_WrongToken(t *testing.T) {
	cfg := newTestConfig()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))

	form := url.Values{"token": {"wrong-token"}}
	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode())), cfg)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Fatalf("wrong token must not set a session cookie")
	}
}

func TestAdminLoginPost_CorrectToken(t *testing.T) {
	cfg := newTestConfig()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))

	form := url.Values{"token": {cfg.Admin.Token}}
	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode())), cfg)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if len(rr.Result().Cookies()) == 0 {
		t.Fatalf("correct token did not set a session cookie")
	}
}

func TestAdminTransferDelete_RequiresSessionCookie(t *testing.T) {
	cfg := newTestConfig()
	r := newAdminRouter(cfg, newTestStores(t), mustManager(t))

	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/transfers/some-id/delete", nil), cfg)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to login (not authenticated)", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q, want /admin/login", loc)
	}
}

func TestAdminTransferDelete_WithSessionCookieProceeds(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)

	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	transfer, _, _, err := stores.Transfers.GetByDownloadToken(tok)
	if err != nil || transfer == nil {
		t.Fatalf("lookup fixture transfer: %v", err)
	}

	cookie := adminSessionCookie(t, cfg)
	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/transfers/"+transfer.ID+"/delete", nil), cfg)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (delete succeeded)", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/admin" {
		t.Fatalf("Location = %q, want /admin", loc)
	}

	deleted, err := stores.Transfers.GetByID(transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == nil || deleted.Status != "deleted" {
		t.Fatalf("transfer status = %+v, want deleted", deleted)
	}
}
