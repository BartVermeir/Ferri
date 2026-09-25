package handler

// HTTP-level tests for the /ul/:token auth gates: unknown token, correct
// token, and password-protected upload requests requiring the upload
// password cookie before the page, the completed-files listing, or the
// file bytes are served.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

func newUploadRouter(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.Handler {
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Get("/ul/{token}", UploadPage(cfg, stores))
	r.Post("/ul/{token}", UploadPassword(cfg, stores))
	r.Post("/ul/{token}/complete", UploadComplete(cfg, stores))
	r.Get("/ul/{token}/files", RequestDownloadPage(cfg, stores))
	r.Post("/ul/{token}/files", RequestFilesPassword(cfg, stores))
	r.Get("/ul/{token}/file/{fileID}", RequestDownloadFile(cfg, stores, mgr))
	r.Get("/ul/{token}/zip", RequestDownloadZIP(cfg, stores, mgr))
	return r
}

// mustCreateUploadRequestWithFile creates an open upload request with one
// completed file, mirroring what the TUS completion hook does in production.
// passwordHash may be empty for an unprotected request.
func mustCreateUploadRequestWithFile(t *testing.T, stores *store.Stores, root, passwordHash string) (uploadToken, fileID string, content []byte) {
	t.Helper()
	content = []byte("received bytes")
	storagePath := "requests/fixed/received.bin"

	requestID, uploadToken, err := stores.Requests.Create(store.CreateRequestInput{
		Title:          "Test request",
		RequesterName:  "Alice",
		RequesterEmail: "alice@example.com",
		PasswordHash:   passwordHash,
		ExpiresAt:      time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	fileID = "fixed-file-id"
	if err := stores.Requests.CreateFileRow(fileID, requestID, "received.bin", storagePath, int64(len(content))); err != nil {
		t.Fatalf("create request file row: %v", err)
	}
	writeStorageFile(t, root, storagePath, content)
	if err := stores.Requests.SetFileComplete(fileID, int64(len(content))); err != nil {
		t.Fatalf("set file complete: %v", err)
	}

	return uploadToken, fileID, content
}

// viewTokenOf returns the requester's view token of an open request. The
// requester routes (/files, /file, /zip) only accept that token, not the
// upload token the external party has (audit M1).
func viewTokenOf(t *testing.T, stores *store.Stores, uploadToken string) string {
	t.Helper()
	req, err := stores.Requests.GetLiveByUploadToken(uploadToken)
	if err != nil || req == nil {
		t.Fatalf("look up request: %v", err)
	}
	if !req.ViewToken.Valid || req.ViewToken.String == "" || req.ViewToken.String == uploadToken {
		t.Fatalf("request has no separate view token: %+v", req.ViewToken)
	}
	return req.ViewToken.String
}

// assertUploadNotFoundPage checks the 404 for dead upload links. It must not
// be the thank-you page: that told uploaders their files had been received.
func assertUploadNotFoundPage(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	assertNotFoundPage(t, rr)
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if body := rr.Body.String(); strings.Contains(body, "have been received") {
		t.Fatalf("dead upload link shows the thank-you page: %s", body)
	}
}

func TestUploadRoutes_UnknownToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	for _, path := range []string{"/ul/does-not-exist", "/ul/does-not-exist/files"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			assertUploadNotFoundPage(t, rr)
		})
	}
}

func TestUploadPage_ExpiredRequest(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	tok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	if _, err := d.Exec(`UPDATE upload_requests SET expires_at = unixepoch() - 60`); err != nil {
		t.Fatal(err)
	}
	r := newUploadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+tok, nil))
	assertUploadNotFoundPage(t, rr)
}

func TestUploadPage_ValidToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	tok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	r := newUploadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/ul/"+tok, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadPage_PasswordProtectedGatesForm(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
	r := newUploadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/ul/"+tok, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (password prompt)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Password required") {
		t.Errorf("expected password prompt, got: %s", rr.Body.String())
	}
}

func TestRequestDownloadPage_PasswordGatesFileListing(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
	tok := viewTokenOf(t, stores, uploadTok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	// Without the cookie: password prompt, not the received file list.
	req := httptest.NewRequest(http.MethodGet, "/ul/"+tok+"/files", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (password prompt)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "received.bin") {
		t.Fatalf("file list leaked without password cookie: %s", rr.Body.String())
	}

	// With the correct password submitted first, the cookie unlocks the listing.
	form := url.Values{"password": {"s3cret"}}
	postReq := httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/files", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRR := httptest.NewRecorder()
	r.ServeHTTP(postRR, postReq)
	if postRR.Code != http.StatusSeeOther {
		t.Fatalf("password post: status = %d, want 303", postRR.Code)
	}
	cookies := postRR.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("correct password did not set a cookie")
	}

	req = httptest.NewRequest(http.MethodGet, "/ul/"+tok+"/files", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated listing: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "received.bin") {
		t.Errorf("expected file list to contain the uploaded filename, got: %s", rr.Body.String())
	}
}

func TestRequestDownloadFile_RequiresPasswordCookie(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	uploadTok, fileID, content := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
	tok := viewTokenOf(t, stores, uploadTok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	// No cookie: redirected, bytes not served.
	req := httptest.NewRequest(http.MethodGet, "/ul/"+tok+"/file/"+fileID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", rr.Code)
	}

	// Correct password cookie: bytes served.
	form := url.Values{"password": {"s3cret"}}
	postReq := httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/files", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRR := httptest.NewRecorder()
	r.ServeHTTP(postRR, postReq)
	cookies := postRR.Result().Cookies()

	req = httptest.NewRequest(http.MethodGet, "/ul/"+tok+"/file/"+fileID, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != string(content) {
		t.Fatalf("body = %q, want %q", rr.Body.String(), content)
	}
}

func TestUploadComplete_RequiresAtLeastOneFile(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	requestID, tok, err := stores.Requests.Create(store.CreateRequestInput{
		Title:          "Empty request",
		RequesterEmail: "alice@example.com",
		ExpiresAt:      time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	r := newUploadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/complete", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-renders upload form)", rr.Code)
	}

	// The request must still be open (not completed) — no file was uploaded.
	reopened, err := stores.Requests.GetByUploadToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if reopened == nil || reopened.Status != "open" {
		t.Fatalf("request %s should remain open with zero files", requestID)
	}
}

// postPassword submits the password form to path, the way the browser does
// when the form (which has no action attribute) posts back to its own URL.
func postPassword(r http.Handler, path, password string) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// A password-protected request must stay reachable for the requester after the
// uploader completed it. The requester arrives via the mail link to /files,
// without a cookie, and the password form posts back to /files. That POST used
// to 405, and POST /ul/:token 404s once the request is no longer open.
func TestRequestFiles_PasswordUnlocksCompletedRequest(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tok, fileID, content := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
	viewTok := viewTokenOf(t, stores, tok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	// Uploader: unlock, then complete.
	uploaderCookies := postPassword(r, "/ul/"+tok, "s3cret").Result().Cookies()
	complete := httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/complete", nil)
	for _, c := range uploaderCookies {
		complete.AddCookie(c)
	}
	r.ServeHTTP(httptest.NewRecorder(), complete)
	if req, _ := stores.Requests.GetByUploadToken(tok); req != nil {
		t.Fatalf("request should be completed, still open")
	}

	// Requester, fresh browser: the listing asks for the password.
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+viewTok+"/files", nil))
	if !strings.Contains(rr.Body.String(), "Password required") {
		t.Fatalf("expected password prompt, got: %s", rr.Body.String())
	}

	// Wrong password: no cookie, prompt again.
	wrong := postPassword(r, "/ul/"+viewTok+"/files", "nope")
	if wrong.Code != http.StatusOK || len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("wrong password: status = %d, cookies = %d, want 200 and none", wrong.Code, len(wrong.Result().Cookies()))
	}
	if !strings.Contains(wrong.Body.String(), "Incorrect password") {
		t.Errorf("expected error message, got: %s", wrong.Body.String())
	}

	// Right password: redirect back to the listing with a cookie.
	right := postPassword(r, "/ul/"+viewTok+"/files", "s3cret")
	if right.Code != http.StatusSeeOther {
		t.Fatalf("correct password: status = %d, want 303", right.Code)
	}
	if loc := right.Header().Get("Location"); loc != "/ul/"+viewTok+"/files" {
		t.Errorf("redirect = %q, want /ul/%s/files", loc, viewTok)
	}
	cookies := right.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("correct password did not set a cookie")
	}

	// The cookie unlocks both the listing and the file bytes.
	list := httptest.NewRequest(http.MethodGet, "/ul/"+viewTok+"/files", nil)
	file := httptest.NewRequest(http.MethodGet, "/ul/"+viewTok+"/file/"+fileID, nil)
	for _, c := range cookies {
		list.AddCookie(c)
		file.AddCookie(c)
	}
	listRR := httptest.NewRecorder()
	r.ServeHTTP(listRR, list)
	if !strings.Contains(listRR.Body.String(), "received.bin") {
		t.Fatalf("listing: expected the file, got: %s", listRR.Body.String())
	}
	fileRR := httptest.NewRecorder()
	r.ServeHTTP(fileRR, file)
	if fileRR.Code != http.StatusOK || fileRR.Body.String() != string(content) {
		t.Fatalf("file: status = %d, body = %q, want 200 and %q", fileRR.Code, fileRR.Body.String(), content)
	}
}

// The requester routes must stop serving files at expires_at, as the mails
// promise. They used to look the request up regardless of status or expiry, so
// the files stayed downloadable until the cleanup job removed them (expiry +
// grace period + up to one cleanup interval, ~30 hours by default).
func TestRequesterRoutes_StopAtExpiry(t *testing.T) {
	cases := []struct {
		name   string
		update string
	}{
		{"completed and past expiry", `UPDATE upload_requests SET status = 'completed', completed_at = unixepoch(), expires_at = unixepoch() - 60`},
		{"marked expired by the job", `UPDATE upload_requests SET status = 'expired', expired_at = unixepoch(), expires_at = unixepoch() - 60`},
		{"deleted by an admin", `UPDATE upload_requests SET status = 'deleted'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDB(t)
			stores := store.New(d)
			mgr, root := newTestManager(t)
			uploadTok, fileID, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
			tok := viewTokenOf(t, stores, uploadTok)
			if _, err := d.Exec(tc.update); err != nil {
				t.Fatal(err)
			}
			r := newUploadRouter(newTestConfig(), stores, mgr)

			for _, path := range []string{"/ul/" + tok + "/files", "/ul/" + tok + "/file/" + fileID, "/ul/" + tok + "/zip"} {
				rr := httptest.NewRecorder()
				r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusNotFound {
					t.Errorf("GET %s: status = %d, want 404", path, rr.Code)
				}
				if strings.Contains(rr.Body.String(), "received bytes") || strings.Contains(rr.Body.String(), "received.bin") {
					t.Errorf("GET %s leaked the file after expiry: %s", path, rr.Body.String())
				}
			}
			if rr := postPassword(r, "/ul/"+tok+"/files", "anything"); rr.Code != http.StatusNotFound {
				t.Errorf("POST /files: status = %d, want 404", rr.Code)
			}
		})
	}
}

// Completed but not yet expired: the requester still gets the files.
func TestRequesterRoutes_CompletedBeforeExpiry(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	uploadTok, fileID, content := mustCreateUploadRequestWithFile(t, stores, root, "")
	tok := viewTokenOf(t, stores, uploadTok)
	if _, err := d.Exec(`UPDATE upload_requests SET status = 'completed', completed_at = unixepoch()`); err != nil {
		t.Fatal(err)
	}
	r := newUploadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+tok+"/file/"+fileID, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != string(content) {
		t.Fatalf("status = %d, body = %q, want 200 and %q", rr.Code, rr.Body.String(), content)
	}
}

// mustOpenRequestWithUploadingFile creates an open request whose only file is
// still 'uploading', as it is while the TUS completion hook is busy.
func mustOpenRequestWithUploadingFile(t *testing.T, stores *store.Stores) (tok, fileID string) {
	t.Helper()
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	requestID, tok, err := stores.Requests.Create(store.CreateRequestInput{
		Title: "Project", RequesterName: "Alice", RequesterEmail: "alice@example.com",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID = "last-file"
	if err := stores.Requests.CreateFileRow(fileID, requestID, "last.mov", "requests/x/last", 5); err != nil {
		t.Fatal(err)
	}
	return tok, fileID
}

func receivedMail(t *testing.T, stores *store.Stores) store.MailItem {
	t.Helper()
	items, err := stores.Mail.FetchPending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d mails, want 1 (files received)", len(items))
	}
	return items[0]
}

// The browser submits /complete right after the last PATCH is answered, while
// the TUS hook may still be marking that file complete. The mail must wait for
// it instead of leaving the last file out.
func TestUploadComplete_WaitsForLastFileToSettle(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	tok, fileID := mustOpenRequestWithUploadingFile(t, stores)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	go func() {
		time.Sleep(100 * time.Millisecond) // the TUS hook, a little late
		stores.Requests.SetFileComplete(fileID, 5)
	}()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/complete", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	m := receivedMail(t, stores)
	if !strings.Contains(m.BodyText, "last.mov") || !strings.Contains(m.Subject, "1 file") {
		t.Fatalf("mail misses the last file: subject %q, body %q", m.Subject, m.BodyText)
	}
}

// A row that never settles (an abandoned attempt) must not hang the request:
// after the timeout the request completes and the mail lists what is complete.
func TestUploadComplete_GivesUpAfterTimeout(t *testing.T) {
	old := requestFilesSettleTimeout
	requestFilesSettleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { requestFilesSettleTimeout = old })

	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	tok, _ := mustOpenRequestWithUploadingFile(t, stores)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	start := time.Now()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/ul/"+tok+"/complete", nil))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("complete took %v, the wait is not bounded", took)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if open, _ := stores.Requests.GetByUploadToken(tok); open != nil {
		t.Fatal("request should be completed after the timeout")
	}
	if m := receivedMail(t, stores); strings.Contains(m.BodyText, "last.mov") {
		t.Fatalf("unfinished file listed in the mail: %q", m.BodyText)
	}
}

// ── Separate upload and view tokens (audit M1) ───────────────────────────────

// The upload link goes to external parties. It must not open the received
// files: with several uploaders on one link, each could download the others'.
func TestRequesterRoutes_RejectUploadToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	uploadTok, fileID, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	viewTok := viewTokenOf(t, stores, uploadTok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	for _, path := range []string{"/ul/" + uploadTok + "/files", "/ul/" + uploadTok + "/file/" + fileID, "/ul/" + uploadTok + "/zip"} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "received") {
			t.Errorf("GET %s with the upload token: status = %d, want 404 without file data", path, rr.Code)
		}
	}
	if rr := postPassword(r, "/ul/"+uploadTok+"/files", "x"); rr.Code != http.StatusNotFound {
		t.Errorf("POST /files with the upload token: status = %d, want 404", rr.Code)
	}

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+viewTok+"/files", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "received.bin") {
		t.Fatalf("GET /files with the view token: status = %d, want 200 with the file", rr.Code)
	}
}

// The view token is for looking only: no upload page, no completing.
func TestViewToken_CannotUploadOrComplete(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	viewTok := viewTokenOf(t, stores, uploadTok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+viewTok, nil))
	assertUploadNotFoundPage(t, rr)

	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/ul/"+viewTok+"/complete", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("POST /complete with the view token: status = %d, want 404", rr.Code)
	}
	if _, ok, err := stores.Requests.ValidateForTUS(viewTok); err != nil || ok {
		t.Errorf("TUS accepted the view token (ok=%v, err=%v)", ok, err)
	}
	if open, _ := stores.Requests.GetByUploadToken(uploadTok); open == nil {
		t.Error("request got completed through the view token")
	}
}

// Requests from before migration 005 have no view token; their upload token
// keeps opening /files so links in mails already sent keep working.
func TestRequesterRoutes_LegacyRequestWithoutViewToken(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	if _, err := d.Exec(`UPDATE upload_requests SET view_token = NULL`); err != nil {
		t.Fatal(err)
	}
	r := newUploadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+uploadTok+"/files", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "received.bin") {
		t.Fatalf("legacy request: status = %d, want 200 with the file", rr.Code)
	}
}

// The "files received" mail carries the view link, never the upload link.
func TestUploadCompleteMail_UsesViewToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	viewTok := viewTokenOf(t, stores, uploadTok)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/ul/"+uploadTok+"/complete", nil))

	m := receivedMail(t, stores)
	for _, body := range []string{m.BodyHTML, m.BodyText} {
		if !strings.Contains(body, "/ul/"+viewTok+"/files") {
			t.Errorf("mail lacks the view link: %s", body)
		}
		if strings.Contains(body, uploadTok) {
			t.Errorf("mail exposes the upload token: %s", body)
		}
	}
}

// After completing, the uploader reopening their link gets the thank-you page
// (not "expired or does not exist", and not the received files).
func TestUploadPage_CompletedShowsThankYou(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	uploadTok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
	if _, err := d.Exec(`UPDATE upload_requests SET status = 'completed', completed_at = unixepoch()`); err != nil {
		t.Fatal(err)
	}
	r := newUploadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+uploadTok, nil))
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "have been received") {
		t.Fatalf("status = %d, want 200 thank-you page, got: %s", rr.Code, body)
	}
	if strings.Contains(body, "received.bin") || strings.Contains(body, "Password required") {
		t.Fatalf("thank-you page shows files or asks for a password: %s", body)
	}

	// Submitting the upload password on a finished request lands there too.
	if rr := postPassword(r, "/ul/"+uploadTok, "s3cret"); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/ul/"+uploadTok {
		t.Errorf("password post on completed request: status = %d, Location = %q", rr.Code, rr.Header().Get("Location"))
	}
}

// The upload page for external parties carries the same limits and the
// folder button (audit M12).
func TestUploadPage_CarriesLimits(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	tok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, "")
	r := newUploadRouter(newTestConfig(), stores, mgr)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ul/"+tok, nil))
	body := rr.Body.String()
	for _, want := range []string{`data-max-files="50"`, `data-max-bytes="644245094400"`, `id="folder-btn"`} {
		if !strings.Contains(body, want) {
			t.Errorf("upload page lacks %s", want)
		}
	}
}
