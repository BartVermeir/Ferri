package handler

// HTTP-level tests for the /dl/:token auth gates: unknown token, correct
// token, and password-protected transfers requiring the download password
// cookie before the page or the file bytes are served.

import (
	"database/sql"
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

// assertNotFoundPage checks the branded 404 page: the right status and text,
// and no template error leaking into the body (the page used to render
// download.html without a Transfer and print the Go error to the visitor).
func assertNotFoundPage(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "Template error") {
		t.Fatalf("template error leaked into the page: %s", body)
	}
	if !strings.Contains(body, "expired or does not exist") {
		t.Errorf("expected the not-found message, got: %s", body)
	}
}

func TestDownloadRoutes_UnknownToken(t *testing.T) {
	stores := newTestStores(t)
	mgr, _ := newTestManager(t)
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	for _, path := range []string{"/dl/does-not-exist", "/dl/does-not-exist/file/x", "/dl/does-not-exist/zip"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			assertNotFoundPage(t, rr)
		})
	}
}

func TestDownloadPage_ExpiredTransfer(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	tok, _, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	if _, err := d.Exec(`UPDATE transfers SET expires_at = unixepoch() - 60`); err != nil {
		t.Fatal(err)
	}
	r := newDownloadRouter(newTestConfig(), stores, mgr)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/dl/"+tok, nil))
	assertNotFoundPage(t, rr)
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

// ── Download counting and notifications (audit M2) ───────────────────────────

type dlFixture struct {
	t       *testing.T
	d       *sql.DB
	r       http.Handler
	tok     string
	fileID  string
	content []byte
}

func newDLFixture(t *testing.T) *dlFixture {
	t.Helper()
	d := newTestDB(t)
	stores := store.New(d)
	mgr, root := newTestManager(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	tok, fileID, _, content := mustCreateActiveTransfer(t, stores, root, "")
	return &dlFixture{t: t, d: d, r: newDownloadRouter(newTestConfig(), stores, mgr), tok: tok, fileID: fileID, content: content}
}

func (f *dlFixture) get(path, rangeHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rr := httptest.NewRecorder()
	f.r.ServeHTTP(rr, req)
	return rr
}

// counts returns (download events, download notification mails).
func (f *dlFixture) counts() (events, mails int) {
	f.t.Helper()
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM download_events`).Scan(&events); err != nil {
		f.t.Fatal(err)
	}
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM mail_queue WHERE subject LIKE 'Downloaded:%'`).Scan(&mails); err != nil {
		f.t.Fatal(err)
	}
	return events, mails
}

// Each Range request used to count as a download and mail the sender: a
// resumed or chunked download of one big file meant dozens of mails.
func TestDownloadFile_ResumeIsNotANewDownload(t *testing.T) {
	f := newDLFixture(t)
	path := "/dl/" + f.tok + "/file/" + f.fileID

	if rr := f.get(path, ""); rr.Code != http.StatusOK {
		t.Fatalf("full GET: status = %d", rr.Code)
	}
	for _, rng := range []string{"bytes=3-", "bytes=5-7", "bytes=-2"} {
		if rr := f.get(path, rng); rr.Code != http.StatusPartialContent {
			t.Fatalf("GET %s: status = %d, want 206", rng, rr.Code)
		}
	}
	if events, mails := f.counts(); events != 1 || mails != 1 {
		t.Fatalf("after 1 download + 3 resumes: events = %d, mails = %d, want 1 and 1", events, mails)
	}
}

// A download starting at byte 0 is a download, whether or not it has a Range.
func TestDownloadFile_RangeFromZeroCounts(t *testing.T) {
	f := newDLFixture(t)
	if rr := f.get("/dl/"+f.tok+"/file/"+f.fileID, "bytes=0-"); rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rr.Code)
	}
	if events, mails := f.counts(); events != 1 || mails != 1 {
		t.Fatalf("events = %d, mails = %d, want 1 and 1", events, mails)
	}
}

// Downloading the same file again within the hour is recorded (the expiry
// summary lists every download) but mails the sender only once.
func TestDownloadFile_RepeatWithinHourMailsOnce(t *testing.T) {
	f := newDLFixture(t)
	path := "/dl/" + f.tok + "/file/" + f.fileID
	for i := 0; i < 5; i++ {
		f.get(path, "")
	}
	if events, mails := f.counts(); events != 5 || mails != 1 {
		t.Fatalf("5 full downloads: events = %d, mails = %d, want 5 and 1", events, mails)
	}

	// Once the last download is older than the window, the next one mails again.
	if _, err := f.d.Exec(`UPDATE download_events SET downloaded_at = unixepoch() - 7200`); err != nil {
		t.Fatal(err)
	}
	f.get(path, "")
	if _, mails := f.counts(); mails != 2 {
		t.Fatalf("download after the window: mails = %d, want 2", mails)
	}
}

// A repeated ZIP download mails once too.
func TestDownloadZIP_RepeatWithinHourMailsOnce(t *testing.T) {
	f := newDLFixture(t)
	for i := 0; i < 3; i++ {
		if rr := f.get("/dl/"+f.tok+"/zip", ""); rr.Code != http.StatusOK {
			t.Fatalf("zip: status = %d", rr.Code)
		}
	}
	if events, mails := f.counts(); events != 3 || mails != 1 {
		t.Fatalf("3 ZIP downloads of 1 file: events = %d, mails = %d, want 3 and 1", events, mails)
	}
}
