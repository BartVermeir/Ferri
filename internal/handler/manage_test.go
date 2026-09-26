package handler

// Tests for the manage page (DEC-043): the sender or requester sees what
// happened, extends or deletes, and the manage link is handed out in the
// /send response, on the "upload link created" page and in the mails.

import (
	"encoding/json"
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

// newManageRouter mirrors the manage routes in cmd/server/routes.go, without
// the IP allowlist (covered by router_test.go).
func newManageRouter(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.Handler {
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Use(appMiddleware.CSRFProtect(cfg.Server.BaseURL))
	r.Get("/manage/{token}", ManagePage(cfg, stores))
	r.Post("/manage/{token}/extend", ManageExtend(cfg, stores))
	r.Post("/manage/{token}/delete", ManageDelete(cfg, stores, mgr, jobs.NewScheduler(cfg, stores, mgr)))
	return r
}

func manageDo(t *testing.T, r http.Handler, cfg *config.Config, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", cfg.Server.BaseURL)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// liveTransfer creates an active transfer with two complete files for bob and
// carol, plus the sender's own link.
func liveTransfer(t *testing.T, stores *store.Stores) *store.CreateTransferResult {
	t.Helper()
	res, err := stores.Transfers.Create(store.CreateTransferInput{
		Title: "Week 23", SenderName: "Alice", SenderEmail: "alice@example.com",
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com", "carol@example.com"},
		Files: []store.CreateFileInput{
			{OriginalName: "a.mov", StoragePath: "transfers/x/a", SizeBytes: 10},
			{OriginalName: "b.mov", StoragePath: "transfers/x/b", SizeBytes: 20},
		},
		NotifyRecipients: true,
		SenderLink:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if err := stores.Transfers.SetFileComplete(f.FileID, 10); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := stores.Transfers.TryActivate(res.TransferID); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	if res.ManageToken == "" {
		t.Fatal("transfer has no manage token")
	}
	return res
}

func TestManageTransfer_ShowsWhoDownloadedWhat(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	mgr, _ := newTestManager(t)
	res := liveTransfer(t, stores)
	var bob string
	for _, rc := range res.Recipients {
		if rc.Email == "bob@example.com" {
			bob = rc.RecipientID
		}
	}
	if _, _, err := stores.Downloads.RecordDownload(bob, res.Files[0].FileID, "a.mov", "", "", time.Hour); err != nil {
		t.Fatal(err)
	}

	rr := manageDo(t, newManageRouter(cfg, stores, mgr), cfg, http.MethodGet, "/manage/"+res.ManageToken, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Week 23", "bob@example.com", "1 of 2 files", "carol@example.com", "Not downloaded yet", "You (your own link)", "a.mov", "b.mov"} {
		if !strings.Contains(body, want) {
			t.Errorf("manage page misses %q", want)
		}
	}
	// Offered: only options that end later than now + 24 h.
	if strings.Contains(body, `<option value="24">`) || !strings.Contains(body, `<option value="168">`) {
		t.Errorf("extend options wrong: want 1 week and longer, not 1 day")
	}
}

func TestManageExtend_OnlyLater(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	mgr, _ := newTestManager(t)
	res := liveTransfer(t, stores)
	r := newManageRouter(cfg, stores, mgr)
	path := "/manage/" + res.ManageToken + "/extend"

	rr := manageDo(t, r, cfg, http.MethodPost, path, url.Values{"expiry_hours": {"168"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/manage/"+res.ManageToken+"?extended=1" {
		t.Fatalf("extend: status = %d, location = %q", rr.Code, rr.Header().Get("Location"))
	}
	tr, _ := stores.Transfers.GetByID(res.TransferID)
	if d := time.Until(tr.ExpiresAt); d < 167*time.Hour || d > 169*time.Hour {
		t.Fatalf("expires in %v, want about a week", d)
	}

	if rr := manageDo(t, r, cfg, http.MethodPost, path, url.Values{"expiry_hours": {"24"}}); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "would not make it available any longer") {
		t.Fatalf("shorter: status = %d, want 400 with a message", rr.Code)
	}
	if rr := manageDo(t, r, cfg, http.MethodPost, path, url.Values{"expiry_hours": {"5"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("option not in expiry_options: status = %d, want 400", rr.Code)
	}
	after, _ := stores.Transfers.GetByID(res.TransferID)
	if !after.ExpiresAt.Equal(tr.ExpiresAt) {
		t.Fatal("a refused extend changed the expiry")
	}

	page := manageDo(t, r, cfg, http.MethodGet, "/manage/"+res.ManageToken+"?extended=1", nil)
	if !strings.Contains(page.Body.String(), "Extended.") {
		t.Error("page after extend does not confirm it")
	}
}

func TestManageDelete_TransferGoneAndSenderSummary(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	mgr, root := newTestManager(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	res := liveTransfer(t, stores)
	if err := stores.Transfers.SetTUSUploadID(res.Files[0].FileID, "tus-a"); err != nil {
		t.Fatal(err)
	}
	writeStorageFile(t, root, "tus-a", []byte("data"))
	r := newManageRouter(cfg, stores, mgr)

	if rr := manageDo(t, r, cfg, http.MethodPost, "/manage/"+res.ManageToken+"/delete", nil); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), "Transfer deleted") {
		t.Fatalf("delete: status = %d", rr.Code)
	}
	tr, _ := stores.Transfers.GetByID(res.TransferID)
	if tr.Status != "deleted" {
		t.Fatalf("status = %q, want deleted", tr.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "tus-a")); !os.IsNotExist(err) {
		t.Fatalf("file still on storage: %v", err)
	}
	items, _ := stores.Mail.FetchPending(10)
	if len(items) != 1 || items[0].Subject != `Deleted: "Week 23"` || !strings.Contains(items[0].BodyText, "was deleted by you") {
		t.Fatalf("want one summary to the sender saying it was deleted by you, got %+v", items)
	}
	if rr := manageDo(t, r, cfg, http.MethodGet, "/manage/"+res.ManageToken, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("manage page after delete: status = %d, want 404", rr.Code)
	}
}

func TestManageRequest_ExtendResetsReminderAndDelete(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	mgr, _ := newTestManager(t)
	id, uploadTok, err := stores.Requests.Create(store.CreateRequestInput{
		Title: "Logos", RequesterName: "Alice", RequesterEmail: "alice@example.com",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := stores.Requests.GetByUploadToken(uploadTok)
	manageTok := req.ManageToken.String
	if manageTok == "" {
		t.Fatal("request has no manage token")
	}
	r := newManageRouter(cfg, stores, mgr)

	page := manageDo(t, r, cfg, http.MethodGet, "/manage/"+manageTok, nil).Body.String()
	for _, want := range []string{"Logos", "/ul/" + uploadTok, "/ul/" + req.ViewPathToken() + "/files", "Nothing received yet"} {
		if !strings.Contains(page, want) {
			t.Errorf("request manage page misses %q", want)
		}
	}

	if ok, err := stores.Requests.ClaimReminder(id); err != nil || !ok {
		t.Fatalf("claim reminder: %v %v", ok, err)
	}
	if rr := manageDo(t, r, cfg, http.MethodPost, "/manage/"+manageTok+"/extend", url.Values{"expiry_hours": {"168"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("extend: status = %d", rr.Code)
	}
	req, _ = stores.Requests.GetByUploadToken(uploadTok)
	if req.RemindedAt.Valid {
		t.Error("extend kept reminded_at: the new expiry would get no reminder")
	}

	if rr := manageDo(t, r, cfg, http.MethodPost, "/manage/"+manageTok+"/delete", nil); rr.Code != http.StatusOK {
		t.Fatalf("delete: status = %d", rr.Code)
	}
	if req, _ := stores.Requests.GetByUploadToken(uploadTok); req != nil {
		t.Fatal("request still open after delete")
	}
}

func TestManage_ExpiredUnknownAndCrossOrigin(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	mgr, _ := newTestManager(t)
	r := newManageRouter(cfg, stores, mgr)
	res, err := stores.Transfers.Create(store.CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(-time.Hour),
		Recipients: []string{"bob@example.com"}, NotifyRecipients: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{res.ManageToken, "unknown"} {
		rr := manageDo(t, r, cfg, http.MethodGet, "/manage/"+tok, nil)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "manage link has expired") {
			t.Errorf("%s: status = %d, want the 404 page", tok, rr.Code)
		}
	}

	live := liveTransfer(t, stores)
	req := httptest.NewRequest(http.MethodPost, "/manage/"+live.ManageToken+"/delete", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req) // no Origin, no Referer
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete: status = %d, want 403", rr.Code)
	}
	if tr, _ := stores.Transfers.GetByID(live.TransferID); tr.Status != "active" {
		t.Fatal("cross-origin delete went through")
	}
}

// The link is handed out: in the /send response (the only place for a
// link-only transfer), on "upload link created", and in the mails.
func TestManageLink_HandedOut(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	cfg := newTestConfig()

	form := url.Values{
		"sender_email": {"alice@example.com"}, "sender_name": {"Alice"},
		"link_only": {"1"}, "expiry_hours": {"24"},
		"files": {`[{"name":"a.mov","size":1}]`},
	}
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	SendCreate(cfg, stores).ServeHTTP(rr, req)
	var resp struct {
		TransferID string `json:"transfer_id"`
		ManageURL  string `json:"manage_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var tok string
	if err := d.QueryRow(`SELECT manage_token FROM transfers WHERE id = ?`, resp.TransferID).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	if resp.ManageURL != "http://example.com/manage/"+tok {
		t.Fatalf("manage_url = %q, stored token %q", resp.ManageURL, tok)
	}

	form = url.Values{"requester_name": {"Alice"}, "requester_email": {"alice@example.com"}, "title": {"P"}, "expiry_hours": {"24"}}
	req = httptest.NewRequest(http.MethodPost, "/request", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	RequestCreate(cfg, stores).ServeHTTP(rr, req)
	if err := d.QueryRow(`SELECT manage_token FROM upload_requests`).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), "http://example.com/manage/"+tok) {
		t.Fatal("upload link created page lacks the manage link")
	}
}

func TestUploadCompleteMail_HasManageLink(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	req, _ := stores.Requests.GetLiveByUploadToken(uploadTok)
	newUploadRouter(newTestConfig(), stores, mgr).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/ul/"+uploadTok+"/complete", nil))

	m := receivedMail(t, stores)
	for _, body := range []string{m.BodyHTML, m.BodyText} {
		if !strings.Contains(body, "/manage/"+req.ManageToken.String) {
			t.Errorf("files-received mail lacks the manage link: %s", body)
		}
	}
}

// The Alerts field keeps a normalised list, and refuses an invalid address.
func TestAdminSettings_AlertRecipients(t *testing.T) {
	cfg, stores := newTestConfig(), newTestStores(t)
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Post("/admin/settings", AdminSettingsSave(cfg, stores))
	post := func(v string) *httptest.ResponseRecorder {
		form := url.Values{"alerts.recipients": {v}, "branding.primary_color": {"#000000"}, "branding.accent_color": {"#f0c800"}, "branding.bg_color": {"#ffffff"}}
		req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	if rr := post("IT@example.com\nops@example.com, it@example.com"); rr.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := stores.Settings.Get().AlertRecipients; got != "it@example.com, ops@example.com" {
		t.Fatalf("saved %q", got)
	}
	if rr := post("it@example.com, not-an-address"); rr.Code == http.StatusSeeOther || !strings.Contains(rr.Body.String(), "Invalid alert recipient") {
		t.Fatalf("invalid address: status = %d, want the settings page with an error", rr.Code)
	}
	if got := stores.Settings.Get().AlertRecipientList(); len(got) != 2 {
		t.Fatalf("invalid save changed the list: %q", got)
	}
}
