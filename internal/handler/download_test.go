package handler

// HTTP-level tests for the /dl/:token auth gates: unknown token, correct
// token, and password-protected transfers requiring the download password
// cookie before the page or the file bytes are served.

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

func newDownloadRouter(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.Handler {
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Get("/dl/{token}", DownloadPage(cfg, stores))
	r.Post("/dl/{token}", DownloadPassword(cfg, stores))
	r.Get("/dl/{token}/file/{fileID}", DownloadFile(cfg, stores, mgr))
	r.Get("/dl/{token}/zip", DownloadZIP(cfg, stores, mgr))
	return r
}

// mustCreateActiveTransfer creates a transfer with a single complete file and
// activates it, mirroring what the TUS completion hooks do in production.
// passwordHash may be empty for an unprotected transfer.
func mustCreateActiveTransfer(t *testing.T, stores *store.Stores, root, passwordHash string) (downloadToken, fileID, storagePath string, content []byte) {
	t.Helper()
	content = []byte("hello world")
	storagePath = "transfers/fixed/test.txt"

	result, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:            "Test transfer",
		SenderName:       "Alice",
		SenderEmail:      "alice@example.com",
		PasswordHash:     passwordHash,
		ExpiresAt:        time.Now().Add(24 * time.Hour),
		Recipients:       []string{"bob@example.com"},
		Files:            []store.CreateFileInput{{OriginalName: "test.txt", StoragePath: storagePath, SizeBytes: int64(len(content))}},
		NotifyRecipients: true,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	fileID = result.Files[0].FileID
	downloadToken = result.Recipients[0].DownloadToken

	writeStorageFile(t, root, storagePath, content)

	if err := stores.Transfers.SetFileComplete(fileID, int64(len(content))); err != nil {
		t.Fatalf("set file complete: %v", err)
	}
	activated, err := stores.Transfers.TryActivate(result.TransferID)
	if err != nil {
		t.Fatalf("try activate: %v", err)
	}
	if !activated {
		t.Fatalf("expected transfer to activate")
	}
	return downloadToken, fileID, storagePath, content
}

func TestDownloadPage_UnknownToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/dl/does-not-exist", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDownloadPage_ValidToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/dl/"+tok, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "test.txt") {
		t.Errorf("expected file list to contain the uploaded filename, got: %s", rr.Body.String())
	}
}

func TestDownloadPage_PasswordProtectedGatesFileListing(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, string(hash))
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	// Without the password cookie: the password prompt is rendered, not the file list.
	req := httptest.NewRequest(http.MethodGet, "/dl/"+tok, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (password prompt)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "test.txt") {
		t.Fatalf("file list leaked without password cookie: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Password required") {
		t.Errorf("expected password prompt, got: %s", rr.Body.String())
	}
}

func TestDownloadPassword_WrongPasswordDoesNotSetCookie(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, string(hash))
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	form := url.Values{"password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/dl/"+tok, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered password form)", rr.Code)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Fatalf("wrong password must not set a cookie, got %v", rr.Result().Cookies())
	}
}

func TestDownloadPassword_CorrectPasswordUnlocksFile(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tok, fileID, _, content := mustCreateActiveTransfer(t, stores, root, string(hash))
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	// A direct file request without the cookie must not serve bytes.
	req := httptest.NewRequest(http.MethodGet, "/dl/"+tok+"/file/"+fileID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated file request: status = %d, want 303 redirect to password page", rr.Code)
	}

	// Submit the correct password.
	form := url.Values{"password": {"s3cret"}}
	req = httptest.NewRequest(http.MethodPost, "/dl/"+tok, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("correct password: status = %d, want 303", rr.Code)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("correct password did not set a cookie")
	}

	// Replay the cookie against the file endpoint.
	req = httptest.NewRequest(http.MethodGet, "/dl/"+tok+"/file/"+fileID, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated file request: status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != string(content) {
		t.Fatalf("body = %q, want %q", rr.Body.String(), content)
	}
}

func TestDownloadFile_UnknownFileID(t *testing.T) {
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	req := httptest.NewRequest(http.MethodGet, "/dl/"+tok+"/file/does-not-exist", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
