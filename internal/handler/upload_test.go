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

func TestUploadPage_UnknownToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	r := newUploadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/ul/does-not-exist", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
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
	tok, _, _ := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
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
	postReq := httptest.NewRequest(http.MethodPost, "/ul/"+tok, strings.NewReader(form.Encode()))
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
	tok, fileID, content := mustCreateUploadRequestWithFile(t, stores, root, string(hash))
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
	postReq := httptest.NewRequest(http.MethodPost, "/ul/"+tok, strings.NewReader(form.Encode()))
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
