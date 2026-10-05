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
// external party and the requester's own view link.
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

// ── Many files ───────────────────────────────────────────────────

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

// The file list travels as one JSON field, so the count of files does not
// run into Go's limit of 1000 multipart parts.
func TestSendCreate_FileListAsJSON(t *testing.T) {
	stores := newTestStores(t)
	code, msg := postSendMultipart(t, stores, func(w *multipart.Writer) {
		w.WriteField("files", `[{"name":"series.zip","size":123456}]`)
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, error = %q", code, msg)
	}
}

// A folder of 1600 files goes as loose files: accepted, with long folder
// paths. Past max_files_per_transfer the
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

	// filenames[]/sizes[] with 3200 parts: Go refuses the form and the error
	// says so.
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

// The filenames[]/sizes[] fields are accepted as well as the files field.
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
	for _, want := range []string{`data-max-files="5000"`, `data-max-bytes="644245094400"`, `id="folder-btn"`, `name="sender_name" autocomplete="name">`, `name="link_count" value="1" min="1" max="50"`} {
		if !strings.Contains(body, want) {
			t.Errorf("send page lacks %s", want)
		}
	}
}

// The password sits under "Additional options" on both panels; "Check upload
// integrity" only shows when upload diagnostics are on.
func TestSendPage_AdditionalOptions(t *testing.T) {
	stores := newTestStores(t)
	for _, on := range []bool{false, true} {
		cfg := newTestConfig()
		cfg.Storage.DiagUploadHashes = on
		rr := httptest.NewRecorder()
		SendPage(cfg, stores).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		body := rr.Body.String()
		if n := strings.Count(body, "<summary>Additional options</summary>\n              <label>Password"); n != 2 {
			t.Errorf("diag %v: password under Additional options %d times, want 2", on, n)
		}
		if got := strings.Contains(body, `id="diag-upload"`); got != on {
			t.Errorf("diag %v: integrity checkbox shown = %v", on, got)
		}
	}
}

// Bodies, names and addresses are bounded, and an address with a line break
// is refused.
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

// The name is optional on both forms; the mails then open with "Hello,".
func TestCreate_NameIsOptional(t *testing.T) {
	stores := newTestStores(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"sender_email": "alice@example.com", "recipients": "bob@example.com",
		"expiry_hours": "24", "files": `[{"name":"a","size":1}]`,
	} {
		w.WriteField(k, v)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/send", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	SendCreate(newTestConfig(), stores).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("send without a name: status %d, body %s", rr.Code, rr.Body.String())
	}

	rr = postRequestForm(t, stores, url.Values{"requester_email": {"alice@example.com"}, "title": {"P"}, "expiry_hours": {"24"}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/ul/") {
		t.Fatalf("request without a name: status %d", rr.Code)
	}
}

func postRequestForm(t *testing.T, stores *store.Stores, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/request", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	RequestCreate(newTestConfig(), stores).ServeHTTP(rr, req)
	return rr
}

// "Number of links" creates that many ordinary requests with the same
// settings, numbered titles and their own tokens, all on one result page.
func TestRequestCreate_MultipleLinks(t *testing.T) {
	d := newTestDB(t)
	stores := store.New(d)
	rr := postRequestForm(t, stores, url.Values{
		"requester_email": {"alice@example.com"}, "title": {"Photos"}, "expiry_hours": {"24"},
		"password": {"secret"}, "link_count": {"3"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()

	rows, err := d.Query(`SELECT title, upload_token, view_token, manage_token, password_hash, expires_at FROM upload_requests ORDER BY title`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var titles []string
	var expiry int64
	seen := map[string]bool{}
	for rows.Next() {
		var title, up, view, manage, hash string
		var exp int64
		if err := rows.Scan(&title, &up, &view, &manage, &hash, &exp); err != nil {
			t.Fatal(err)
		}
		titles = append(titles, title)
		if expiry == 0 {
			expiry = exp
		}
		if exp != expiry || hash == "" {
			t.Errorf("%s: expiry %d (first %d), password hash %q: every link gets the same settings", title, exp, expiry, hash)
		}
		for _, tok := range []string{up, view, manage} {
			if seen[tok] {
				t.Errorf("%s: token shared with another link", title)
			}
			seen[tok] = true
		}
		for _, link := range []string{"/ul/" + up + `"`, "/ul/" + view + "/files", "/manage/" + manage} {
			if !strings.Contains(body, link) {
				t.Errorf("%s: result page lacks %s", title, link)
			}
		}
	}
	if strings.Join(titles, ",") != "Photos #1,Photos #2,Photos #3" {
		t.Fatalf("titles = %v", titles)
	}
	if !strings.Contains(body, "3 upload links created") || !strings.Contains(body, "data-copy-all") {
		t.Error("result page lacks the heading or Copy all links")
	}

	for _, n := range []string{"0", "51", "x"} {
		rr := postRequestForm(t, stores, url.Values{"requester_email": {"alice@example.com"}, "title": {"P"}, "expiry_hours": {"24"}, "link_count": {n}})
		if !strings.Contains(rr.Body.String(), "Number of links must be between 1 and 50") {
			t.Errorf("link_count %s accepted", n)
		}
	}
}
