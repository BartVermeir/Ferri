package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/store"
)

// POST /send must store how many files the browser announced: TryActivate
// waits for that many before the transfer goes live and recipients are mailed.
func TestSendCreate_StoresExpectedFiles(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	cfg := newTestConfig()

	form := url.Values{
		"sender_email": {"alice@example.com"},
		"sender_name":  {"Alice"},
		"recipients":   {"bob@example.com"},
		"expiry_hours": {"24"},
		"filenames[]":  {"one.mov", "two.mov", "three.mov"},
		"sizes[]":      {"1", "2", "3"},
	}
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	SendCreate(cfg, stores).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		TransferID string `json:"transfer_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var expected int
	if err := d.QueryRow(`SELECT expected_files FROM transfers WHERE id = ?`, resp.TransferID).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if expected != 3 {
		t.Fatalf("expected_files = %d, want 3", expected)
	}
}

// "Upload link created" shows two different links: the upload link for the
// external party and the requester's own view link (audit M1).
func TestRequestCreate_ShowsUploadAndViewLink(t *testing.T) {
	stores := newTestStores(t)
	cfg := newTestConfig()
	form := url.Values{
		"requester_name":  {"Alice"},
		"requester_email": {"alice@example.com"},
		"title":           {"Project"},
		"expiry_hours":    {"24"},
	}
	req := httptest.NewRequest(http.MethodPost, "/request", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	RequestCreate(cfg, stores).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	links := regexp.MustCompile(`http://example\.com/ul/([1-9A-HJ-NP-Za-km-z]{44})(/files)?`).FindAllStringSubmatch(rr.Body.String(), -1)
	var uploadTok, viewTok string
	for _, l := range links {
		if l[2] == "" {
			uploadTok = l[1]
		} else {
			viewTok = l[1]
		}
	}
	if uploadTok == "" || viewTok == "" || uploadTok == viewTok {
		t.Fatalf("want a distinct upload and view link, got upload=%q view=%q", uploadTok, viewTok)
	}
	if got := viewTokenOf(t, stores, uploadTok); got != viewTok {
		t.Fatalf("page view token %q, stored %q", viewTok, got)
	}
}

// ── Many files (audit M12) ───────────────────────────────────────────────────

func postSendMultipart(t *testing.T, stores *store.Stores, fields func(w *multipart.Writer)) (int, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"sender_name": "Alice", "sender_email": "alice@example.com",
		"recipients": "bob@example.com", "expiry_hours": "24",
	} {
		w.WriteField(k, v)
	}
	fields(w)
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/send", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	SendCreate(newTestConfig(), stores).ServeHTTP(rr, req)
	var resp struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp.Error
}

// The file list travels as one JSON field, so the count of files no longer
// runs into Go's limit of 1000 multipart parts.
func TestSendCreate_FileListAsJSON(t *testing.T) {
	stores := newTestStores(t)
	code, msg := postSendMultipart(t, stores, func(w *multipart.Writer) {
		w.WriteField("files", `[{"name":"series.zip","size":123456}]`)
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, error = %q", code, msg)
	}
}

// A folder of 1600 files (what a colleague sent, 2026-09-25) goes as loose
// files: accepted, with long folder paths. Past max_files_per_transfer the
// server says so, never "Sender name is required".
func TestSendCreate_ManyFilesGetTheRealError(t *testing.T) {
	stores := newTestStores(t)
	fileList := func(n int) string {
		var list []string
		for i := 0; i < n; i++ {
			list = append(list, fmt.Sprintf(`{"name":"Project %s/day %d/img%05d.jpg","size":1}`, strings.Repeat("x", 120), i%40, i))
		}
		return "[" + strings.Join(list, ",") + "]"
	}

	code, msg := postSendMultipart(t, stores, func(w *multipart.Writer) { w.WriteField("files", fileList(1600)) })
	if code != http.StatusOK {
		t.Errorf("folder of 1600 files: status = %d, error = %q, want it accepted", code, msg)
	}
	code, msg = postSendMultipart(t, stores, func(w *multipart.Writer) { w.WriteField("files", fileList(5000)) })
	if code != http.StatusOK {
		t.Errorf("5000 files with long paths: status = %d, error = %q, want it accepted (body cap)", code, msg)
	}
	code, msg = postSendMultipart(t, stores, func(w *multipart.Writer) { w.WriteField("files", fileList(5001)) })
	if code != http.StatusBadRequest || !strings.Contains(msg, "Maximum 5000 files") {
		t.Errorf("5001 files: status = %d, error = %q, want the file limit", code, msg)
	}

	// Old format, 3200 parts: Go refuses the form; say so.
	code, msg = postSendMultipart(t, stores, func(w *multipart.Writer) {
		for i := 0; i < 1600; i++ {
			w.WriteField("filenames[]", fmt.Sprintf("img%d.jpg", i))
			w.WriteField("sizes[]", "1")
		}
	})
	if code != http.StatusBadRequest || !strings.Contains(msg, "Could not read the form") {
		t.Errorf("old format with 1600 files: status = %d, error = %q, want a form read error", code, msg)
	}
}

// A page that was open during the deploy still sends the old format.
func TestSendCreate_OldFormatStillAccepted(t *testing.T) {
	stores := newTestStores(t)
	code, msg := postSendMultipart(t, stores, func(w *multipart.Writer) {
		w.WriteField("filenames[]", "a.mov")
		w.WriteField("sizes[]", "10")
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, error = %q", code, msg)
	}
}

// The send page carries the limits upload.js checks.
func TestSendPage_CarriesLimits(t *testing.T) {
	stores := newTestStores(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	SendPage(newTestConfig(), stores).ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{`data-max-files="5000"`, `data-max-bytes="644245094400"`, `id="folder-btn"`, `name="sender_name" autocomplete="name" required`} {
		if !strings.Contains(body, want) {
			t.Errorf("send page lacks %s", want)
		}
	}
}

// Audit L11: bodies, names and addresses are bounded, and an address with a
// line break no longer gets in (the mail to it used to fail for good later).
func TestSendCreate_Bounds(t *testing.T) {
	stores := newTestStores(t)
	code, msg := postSendMultipart(t, stores, func(w *multipart.Writer) {
		w.WriteField("message", strings.Repeat("x", maxSendBytes+1))
	})
	if code != http.StatusBadRequest || !strings.Contains(msg, "Could not read the form") {
		t.Errorf("body over %d bytes: status = %d, error = %q", maxSendBytes, code, msg)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"sender_name": strings.Repeat("n", maxNameLen+1), "sender_email": "alice@example.com",
		"recipients": "bob@example.com", "expiry_hours": "24", "files": `[{"name":"a","size":1}]`,
	} {
		w.WriteField(k, v)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/send", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	SendCreate(newTestConfig(), stores).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Name is too long") {
		t.Errorf("long name: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestIsValidEmail_RejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{"alice@example.com\r\nBcc: x@evil.test", "ali ce@example.com", "alice@exa\tmple.com", strings.Repeat("a", 250) + "@example.com"} {
		if isValidEmail(bad) {
			t.Errorf("isValidEmail(%q) = true", bad)
		}
	}
	if !isValidEmail("alice@example.com") {
		t.Error("a normal address is rejected")
	}
}
