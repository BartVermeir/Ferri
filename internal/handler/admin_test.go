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
		r.Get("/admin/transfers/{id}/files", AdminTransferFiles(cfg, stores))
		r.Get("/admin/transfers/{id}/file/{fileID}", AdminTransferFile(cfg, stores, mgr))
		r.Get("/admin/transfers/{id}/stats", AdminTransferStats(cfg, stores))
		r.Get("/admin/requests/{id}/stats", AdminRequestStats(cfg, stores))
		r.Get("/admin/requests/{id}/files", AdminRequestFiles(cfg, stores))
		r.Get("/admin/requests/{id}/file/{fileID}", AdminRequestFile(cfg, stores, mgr))
		r.Post("/admin/transfers/{id}/delete", AdminTransferDelete(cfg, stores, mgr, jobs.NewScheduler(cfg, stores, mgr)))
		r.Post("/admin/requests/{id}/delete", AdminRequestDelete(cfg, stores, mgr))
		r.Post("/admin/delete", AdminBulkDelete(cfg, stores, mgr, jobs.NewScheduler(cfg, stores, mgr)))
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
// token that external parties hold.
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

// A download through a recipient's own link counts as that
// recipient and mails the sender. The dashboard links those links anyway (the
// admin needs them to debug what a recipient sees); the admin's own file page
// serves files without recording anything.
func TestAdminTransferFiles_DownloadIsNotARecipientDownload(t *testing.T) {
	cfg := newTestConfig()
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	_, fileID, _, content := mustCreateActiveTransfer(t, stores, root, "")
	f, err := stores.Transfers.GetFileByID(fileID)
	if err != nil || f == nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	dash := get("/admin").Body.String()
	rcpts, err := stores.Transfers.GetRecipients(f.TransferID)
	if err != nil || len(rcpts) == 0 {
		t.Fatalf("recipients: %v, %d", err, len(rcpts))
	}
	if !strings.Contains(dash, `href="/dl/`+rcpts[0].DownloadToken+`"`) {
		t.Fatal("dashboard does not link the recipient's download link")
	}
	if !strings.Contains(dash, "/admin/transfers/"+f.TransferID+"/files") {
		t.Fatal("dashboard has no link to the transfer's files")
	}
	if page := get("/admin/transfers/" + f.TransferID + "/files").Body.String(); !strings.Contains(page, "test.txt") {
		t.Fatalf("files page does not list the file:\n%s", page)
	}
	rr := get("/admin/transfers/" + f.TransferID + "/file/" + fileID)
	if rr.Code != http.StatusOK || rr.Body.String() != string(content) {
		t.Fatalf("admin download: status %d, body %q", rr.Code, rr.Body.String())
	}
	var events, mails int
	d.QueryRow(`SELECT COUNT(*) FROM download_events`).Scan(&events)
	d.QueryRow(`SELECT COUNT(*) FROM mail_queue WHERE subject LIKE 'Downloaded:%'`).Scan(&mails)
	if events != 0 || mails != 0 {
		t.Fatalf("admin download recorded %d events and queued %d mails, want 0 and 0", events, mails)
	}
	if rr := get("/admin/transfers/other/file/" + fileID); rr.Code != http.StatusNotFound {
		t.Fatalf("file served under another transfer's id: status %d", rr.Code)
	}
}

// Transfers and requests share one list on the dashboard, newest first: a
// request must not sink below every transfer.
func TestAdminDashboard_ListsTransfersAndRequestsByCreation(t *testing.T) {
	cfg := newTestConfig()
	d := newTestDB(t)
	stores := store.New(d)
	mgr, _ := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)

	mkTransfer := func(title string, created time.Time) {
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
		if _, err := d.Exec(`UPDATE transfers SET created_at = ? WHERE id = ?`, created.Unix(), res.TransferID); err != nil {
			t.Fatal(err)
		}
	}
	mkRequest := func(title string, created time.Time) {
		t.Helper()
		id, _, err := stores.Requests.Create(store.CreateRequestInput{
			Title: title, RequesterName: "Alice", RequesterEmail: "alice@example.com",
			ExpiresAt: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE upload_requests SET created_at = ? WHERE id = ?`, created.Unix(), id); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	mkTransfer("send-oldest", now.Add(-4*time.Hour))
	mkRequest("request-old", now.Add(-3*time.Hour))
	mkTransfer("send-new", now.Add(-2*time.Hour))
	mkRequest("request-newest", now.Add(-1*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(adminSessionCookie(t, cfg))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	body := rr.Body.String()

	want := []string{"request-newest", "send-new", "request-old", "send-oldest"}
	last := -1
	for _, title := range want {
		i := strings.Index(body, ">"+title+"<")
		if i < 0 {
			t.Fatalf("%s missing from the dashboard", title)
		}
		if i < last {
			t.Fatalf("order wrong at %s, want %v", title, want)
		}
		last = i
	}
}

// "Delete selected" deletes the ticked transfers and requests, each the
// way its own Delete button would (a live transfer mails the sender the
// summary), and leaves the rest alone. The dashboard offers a checkbox per
// row in the bulk form and reports the count after the redirect.
func TestAdminBulkDelete_DeletesSelectedOnly(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}

	mkTransfer := func(title string) string {
		t.Helper()
		res, err := stores.Transfers.Create(store.CreateTransferInput{
			Title: title, SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
			Recipients:       []string{"bob@example.com"},
			Files:            []store.CreateFileInput{{OriginalName: title + ".mov", StoragePath: "p/" + title, SizeBytes: 1}},
			NotifyRecipients: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := stores.Transfers.SetFileComplete(res.Files[0].FileID, 1); err != nil {
			t.Fatal(err)
		}
		if ok, err := stores.Transfers.TryActivate(res.TransferID); err != nil || !ok {
			t.Fatalf("activate: %v %v", ok, err)
		}
		return res.TransferID
	}
	mkRequest := func() string {
		t.Helper()
		id, _, err := stores.Requests.Create(store.CreateRequestInput{
			RequesterEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	t1, t2, keepT := mkTransfer("one"), mkTransfer("two"), mkTransfer("keep")
	r1, keepR := mkRequest(), mkRequest()
	if err := stores.Requests.CreateFileRow("rf", r1, "b.mov", "requests/"+r1+"/rf", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetTUSUploadID("rf", "tus-request"); err != nil {
		t.Fatal(err)
	}
	writeStorageFile(t, root, "tus-request", []byte("data"))

	get := httptest.NewRequest(http.MethodGet, "/admin", nil)
	get.AddCookie(cookie)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, get)
	body := rr.Body.String()
	for _, want := range []string{
		`action="/admin/delete"`,
		`name="transfer" value="` + t1 + `" form="bulk-delete"`,
		`name="request" value="` + r1 + `" form="bulk-delete"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %s", want)
		}
	}

	// t1 twice and an empty value: deleted once, the empty one ignored.
	form := url.Values{"transfer": {t1, t2, t1, ""}, "request": {r1}}
	req := withOrigin(httptest.NewRequest(http.MethodPost, "/admin/delete", strings.NewReader(form.Encode())), cfg)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin?deleted=3&failed=0" {
		t.Fatalf("status = %d, location = %q, want 303 to /admin?deleted=3&failed=0", rr.Code, rr.Header().Get("Location"))
	}

	for id, want := range map[string]string{t1: "deleted", t2: "deleted", keepT: "active"} {
		tr, err := stores.Transfers.GetByID(id)
		if err != nil || tr == nil || tr.Status != want {
			t.Errorf("transfer %s = %+v (err %v), want %s", id, tr, err, want)
		}
	}
	for id, want := range map[string]string{r1: "deleted", keepR: "open"} {
		ur, err := stores.Requests.GetByID(id)
		if err != nil || ur == nil || ur.Status != want {
			t.Errorf("request %s = %+v (err %v), want %s", id, ur, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "tus-request")); !os.IsNotExist(err) {
		t.Errorf("request file still on storage (stat err = %v)", err)
	}

	pending, err := stores.Mail.FetchPending(50)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, it := range pending {
		if strings.HasPrefix(it.Subject, "Deleted:") {
			subjects = append(subjects, it.Subject)
		}
	}
	if len(subjects) != 2 {
		t.Errorf("deletion summaries = %q, want one for each deleted live transfer", subjects)
	}

	get = httptest.NewRequest(http.MethodGet, "/admin?deleted=3&failed=0", nil)
	get.AddCookie(cookie)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, get)
	if !strings.Contains(rr.Body.String(), "3 item(s) deleted.") {
		t.Error("dashboard does not report the deleted count")
	}
}

// An expired transfer or request whose files are still on storage is
// listed in its own dashboard section (not among the live ones), and its
// files can be downloaded through the admin files page. Once the cleanup job
// has marked the files deleted, or when there never were files, it is gone
// from the section. A request's files page serves only its own files.
func TestAdminDashboard_ExpiredNotCleanedUp(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	_, tFileID, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	tf, err := stores.Transfers.GetFileByID(tFileID)
	if err != nil || tf == nil {
		t.Fatal(err)
	}
	transferID := tf.TransferID
	if err := stores.Transfers.SetExpired(transferID); err != nil {
		t.Fatal(err)
	}
	uploadTok, fileID, content := mustCreateUploadRequestWithFile(t, stores, root, "")
	ur, err := stores.Requests.GetByUploadToken(uploadTok)
	if err != nil || ur == nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetExpired(ur.ID); err != nil {
		t.Fatal(err)
	}
	// Past expiry but the hourly expiry job has not run yet: listed too.
	lateID, _, err := stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "late@example.com", ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.CreateFileRow("late-file", lateID, "late.bin", "requests/"+lateID+"/late-file", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetFileComplete("late-file", 1); err != nil {
		t.Fatal(err)
	}
	// Expired without files, and expired with its files cleaned up: not listed.
	emptyID, _, err := stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "empty@example.com", ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanedID, _, err := stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "cleaned@example.com", ExpiresAt: time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.CreateFileRow("cleaned-file", cleanedID, "gone.bin", "requests/"+cleanedID+"/cleaned-file", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetFileComplete("cleaned-file", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.MarkFilesDeleted(cleanedID); err != nil {
		t.Fatal(err)
	}

	dash := get("/admin").Body.String()
	i := strings.Index(dash, "Expired, not cleaned up yet")
	if i < 0 {
		t.Fatal("dashboard has no expired section")
	}
	live, expired := dash[:i], dash[i:]
	for _, id := range []string{transferID, ur.ID, lateID} {
		if !strings.Contains(expired, "/"+id+"/files") {
			t.Errorf("expired section lacks %s", id)
		}
		if strings.Contains(live, `value="`+id+`"`) {
			t.Errorf("%s is still among the live items", id)
		}
	}
	for _, id := range []string{emptyID, cleanedID} {
		if strings.Contains(dash, id) {
			t.Errorf("%s listed, but it has no files left", id)
		}
	}

	page := get("/admin/requests/" + ur.ID + "/files").Body.String()
	for _, want := range []string{"received.bin", "/admin/requests/" + ur.ID + "/file/" + fileID, "cleanup job removes them"} {
		if !strings.Contains(page, want) {
			t.Errorf("request files page lacks %q", want)
		}
	}
	rr := get("/admin/requests/" + ur.ID + "/file/" + fileID)
	if rr.Code != http.StatusOK || rr.Body.String() != string(content) {
		t.Fatalf("download: status %d, body %q", rr.Code, rr.Body.String())
	}
	if rr := get("/admin/requests/" + lateID + "/file/" + fileID); rr.Code != http.StatusNotFound {
		t.Errorf("file of another request: status %d, want 404", rr.Code)
	}
	if rr := get("/admin/requests/nope/files"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown request: status %d, want 404", rr.Code)
	}
}
