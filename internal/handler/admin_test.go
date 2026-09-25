package handler

// HTTP-level tests for the admin auth gate: routes under /admin require a
// valid signed session cookie; POST routes also go through CSRFProtect,
// mirroring the middleware stack wired in cmd/server/main.go.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/jobs"
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
		r.Post("/admin/transfers/{id}/delete", AdminTransferDelete(cfg, stores, mgr, jobs.NewScheduler(cfg, stores, mgr)))
		r.Post("/admin/requests/{id}/delete", AdminRequestDelete(cfg, stores, mgr))
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

// Admin delete must remove the flat TUS file (<tus_upload_id> + .info), where
// the data really lives, and mark it purged. Both delete buttons, one test.
func TestAdminDelete_RemovesFlatTUSFiles(t *testing.T) {
	cfg := newTestConfig()
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)

	res, err := stores.Transfers.Create(store.CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Transfers.CreateFileRow("tf", res.TransferID, "a.mov", "transfers/"+res.TransferID+"/tf", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Transfers.SetTUSUploadID("tf", "tus-transfer"); err != nil {
		t.Fatal(err)
	}
	requestID, _, err := stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.CreateFileRow("rf", requestID, "b.mov", "requests/"+requestID+"/rf", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetTUSUploadID("rf", "tus-request"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tus-transfer", "tus-transfer.info", "tus-request", "tus-request.info"} {
		writeStorageFile(t, root, name, []byte("data"))
	}

	for _, path := range []string{"/admin/transfers/" + res.TransferID + "/delete", "/admin/requests/" + requestID + "/delete"} {
		req := withOrigin(httptest.NewRequest(http.MethodPost, path, nil), cfg)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("POST %s: status = %d, want 303", path, rr.Code)
		}
	}

	for _, name := range []string{"tus-transfer", "tus-transfer.info", "tus-request", "tus-request.info"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s still on storage (stat err = %v)", name, err)
		}
	}
	left, err := stores.Files.ListUnpurged()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("files not marked purged: %+v", left)
	}
}

// The dashboard's "view files" link must use the view token, not the upload
// token that external parties hold (audit M1).
func TestAdminDashboard_ViewFilesLinkUsesViewToken(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	viewTok := viewTokenOf(t, stores, uploadTok)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(adminSessionCookie(t, cfg))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "/ul/"+viewTok+"/files") {
		t.Fatalf("dashboard lacks the view link, body: %s", body)
	}
	if strings.Contains(body, "/ul/"+uploadTok+"/files") {
		t.Fatalf("dashboard links /files with the upload token")
	}
}

// Deleting a live transfer by hand sends the sender the same "who downloaded
// what" summary an expiry does. Not for an expired transfer (it already had
// one), not for a pending one (it never reached anyone), and not when expiry
// summaries are switched off.
func TestAdminDelete_SendsSummaryForLiveTransferOnly(t *testing.T) {
	cfg := newTestConfig()
	d := newTestDB(t)
	stores := store.New(d)
	mgr, _ := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}

	mk := func(title string, live bool) (transferID, recipientID, fileID string) {
		t.Helper()
		res, err := stores.Transfers.Create(store.CreateTransferInput{
			Title: title, SenderName: "Alice", SenderEmail: "alice@example.com",
			ExpiresAt: time.Now().Add(24 * time.Hour), Recipients: []string{"bob@example.com"},
			Files:            []store.CreateFileInput{{OriginalName: title + ".mov", StoragePath: "p/" + title, SizeBytes: 1}},
			NotifyRecipients: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if live {
			if err := stores.Transfers.SetFileComplete(res.Files[0].FileID, 1); err != nil {
				t.Fatal(err)
			}
			if ok, err := stores.Transfers.TryActivate(res.TransferID); err != nil || !ok {
				t.Fatalf("activate: %v %v", ok, err)
			}
		}
		return res.TransferID, res.Recipients[0].RecipientID, res.Files[0].FileID
	}
	del := func(id string) {
		t.Helper()
		req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/transfers/"+id+"/delete", nil), cfg)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("delete %s: status = %d, want 303", id, rr.Code)
		}
	}
	summaries := func() []store.MailItem {
		t.Helper()
		items, err := stores.Mail.FetchPending(50)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.MailItem
		for _, it := range items {
			if strings.HasPrefix(it.Subject, "Deleted:") || strings.HasPrefix(it.Subject, "Expired:") {
				out = append(out, it)
			}
		}
		return out
	}

	live, bob, file := mk("live", true)
	if _, _, err := stores.Downloads.RecordDownload(bob, file, "live.mov", "", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	expired, _, _ := mk("expired", true)
	if err := stores.Transfers.SetExpired(expired); err != nil {
		t.Fatal(err)
	}
	pending, _, _ := mk("pending", false)

	for _, id := range []string{live, expired, pending} {
		del(id)
	}
	got := summaries()
	if len(got) != 1 || got[0].Subject != `Deleted: "live"` {
		var subjects []string
		for _, it := range got {
			subjects = append(subjects, it.Subject)
		}
		t.Fatalf("summaries = %q, want exactly one, `Deleted: \"live\"`", subjects)
	}
	for _, want := range []string{"was deleted by an administrator", "bob@example.com: downloaded", "live.mov", "The files have been deleted."} {
		if !strings.Contains(got[0].BodyText, want) {
			t.Errorf("summary misses %q:\n%s", want, got[0].BodyText)
		}
	}
	if strings.Contains(got[0].BodyText, "grace period") {
		t.Error("deletion summary talks about a grace period")
	}

	if err := stores.Settings.Save("mail.expiry_summary", "false"); err != nil {
		t.Fatal(err)
	}
	off, _, _ := mk("off", true)
	del(off)
	if n := len(summaries()); n != 1 {
		t.Fatalf("with expiry summaries switched off: %d summaries, want still 1", n)
	}
}
