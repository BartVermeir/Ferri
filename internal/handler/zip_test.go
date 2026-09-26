package handler

// Audit M8 (file names on the requester routes) and M9 (ZIPs that were
// silently incomplete, and partial uploads offered as files).

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

type reqFixture struct {
	t      *testing.T
	stores *store.Stores
	root   string
	mgr    *storage.Manager
	r      http.Handler
	id     string
	view   string
}

func newReqFixture(t *testing.T, title string) *reqFixture {
	t.Helper()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	id, uploadToken, err := stores.Requests.Create(store.CreateRequestInput{
		Title: title, RequesterEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &reqFixture{t: t, stores: stores, root: root, mgr: mgr, r: newUploadRouter(newTestConfig(), stores, mgr), id: id}
	f.view = viewTokenOf(t, stores, uploadToken)
	return f
}

// file adds a request file; complete=false leaves it 'uploading' (a partial
// or broken-off upload). onDisk=false leaves its data missing from storage.
func (f *reqFixture) file(id, name, content string, complete, onDisk bool) {
	f.t.Helper()
	path := "requests/" + f.id + "/" + id
	if err := f.stores.Requests.CreateFileRow(id, f.id, name, path, int64(len(content))); err != nil {
		f.t.Fatal(err)
	}
	if onDisk {
		writeStorageFile(f.t, f.root, path, []byte(content))
	}
	if complete {
		if err := f.stores.Requests.SetFileComplete(id, int64(len(content))); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *reqFixture) get(path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	f.r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func readZIP(t *testing.T, body []byte) map[string]*zip.File {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a valid ZIP: %v", err)
	}
	out := map[string]*zip.File{}
	for _, zf := range zr.File {
		out[zf.Name] = zf
	}
	return out
}

func zipText(t *testing.T, zf *zip.File) string {
	t.Helper()
	rc, err := zf.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// M8: a received file keeps its name. url.QueryEscape made
// "Café scene; v2.mov" into "Caf%C3%A9+scene%3B+v2.mov".
func TestRequestDownloadFile_KeepsFileName(t *testing.T) {
	f := newReqFixture(t, "Test")
	f.file("f1", "Café scene; v2.mov", "data", true, true)

	cd := f.get("/ul/" + f.view + "/file/f1").Header().Get("Content-Disposition")
	if want := buildContentDisposition("Café scene; v2.mov"); cd != want {
		t.Fatalf("Content-Disposition = %q, want %q", cd, want)
	}
	if strings.Contains(cd, "+") {
		t.Fatalf("spaces became '+': %q", cd)
	}
}

// M8: ZIP names. An untitled request gave ".zip"; accents in a transfer
// title came out percent-encoded.
func TestZIPFileNames(t *testing.T) {
	f := newReqFixture(t, "")
	f.file("f1", "a.mov", "data", true, true)
	if cd := f.get("/ul/" + f.view + "/zip").Header().Get("Content-Disposition"); cd != buildContentDisposition("files.zip") {
		t.Fatalf("untitled request ZIP: Content-Disposition = %q", cd)
	}
	if got := zipFileName("Épisode 1: final"); got != "Épisode_1__final.zip" {
		t.Fatalf("zipFileName = %q", got)
	}
}

// M9: a file still (or forever) 'uploading' is not offered to the requester:
// not on the page, not as a download, not in the ZIP.
func TestRequestRoutes_OnlyCompleteFiles(t *testing.T) {
	f := newReqFixture(t, "Test")
	f.file("done", "done.mov", "full", true, true)
	f.file("half", "half.mov", "par", false, true)

	if body := f.get("/ul/" + f.view + "/files").Body.String(); strings.Contains(body, "half.mov") || !strings.Contains(body, "done.mov") {
		t.Fatalf("file page lists the partial upload, or misses the complete one:\n%s", body)
	}
	if rr := f.get("/ul/" + f.view + "/file/half"); rr.Code != http.StatusNotFound {
		t.Fatalf("download of a partial upload: status = %d, want 404", rr.Code)
	}
	entries := readZIP(t, f.get("/ul/"+f.view+"/zip").Body.Bytes())
	if _, ok := entries["half.mov"]; ok || entries["done.mov"] == nil {
		t.Fatalf("ZIP entries = %v, want only done.mov", entries)
	}
	if m := entries["done.mov"].Method; m != zip.Store {
		t.Fatalf("entry method = %d, want Store (%d): video does not compress", m, zip.Store)
	}
}

// M9: a file missing from storage used to vanish from the ZIP without a word.
// Now the ZIP says which files are not in it.
func TestZIP_MissingFileIsNamed(t *testing.T) {
	f := newReqFixture(t, "Test")
	f.file("ok", "ok.mov", "data", true, true)
	f.file("gone", "gone.mov", "data", true, false)

	rr := f.get("/ul/" + f.view + "/zip")
	entries := readZIP(t, rr.Body.Bytes())
	if entries["ok.mov"] == nil {
		t.Fatalf("readable file missing from the ZIP: %v", entries)
	}
	note := entries["MISSING_FILES.txt"]
	if note == nil {
		t.Fatalf("no MISSING_FILES.txt in a ZIP that lacks gone.mov: %v", entries)
	}
	if text := zipText(t, note); !strings.Contains(text, "gone.mov") {
		t.Fatalf("MISSING_FILES.txt does not name gone.mov:\n%s", text)
	}
}

// failingBackend is local storage whose files break off after a few bytes,
// like an SMB session that drops mid-download.
type failingBackend struct{ *storage.LocalBackend }

func (b failingBackend) Open(path string) (io.ReadSeekCloser, error) {
	f, err := b.LocalBackend.Open(path)
	if err != nil {
		return nil, err
	}
	return brokenReader{f}, nil
}

type brokenReader struct{ io.ReadSeekCloser }

func (brokenReader) Read(p []byte) (int, error) { return 0, errors.New("connection reset") }

// M9: a read failure halfway a file must abort the download (the browser
// then shows it failed), not end in a ZIP holding a truncated file.
func TestZIP_ReadErrorAbortsDownload(t *testing.T) {
	f := newReqFixture(t, "Test")
	f.file("f1", "a.mov", "data", true, true)
	f.mgr.Swap(failingBackend{storage.NewLocalBackend(f.root)})

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", rec)
		}
	}()
	rr := f.get("/ul/" + f.view + "/zip")
	t.Fatalf("download completed (status %d, %d bytes) despite the read error", rr.Code, rr.Body.Len())
}

// DEC-035: folder paths become folders in the ZIP; a raw "../" in a stored
// name (a row from before names were cleaned) cannot leave the archive. A
// single download is named after the file, without its folder.
func TestZIP_FolderStructure(t *testing.T) {
	f := newReqFixture(t, "Series")
	f.file("a", "Series/day1/img001.jpg", "one", true, true)
	f.file("b", "Series/day2/img002.jpg", "two", true, true)
	f.file("c", "../../escape.txt", "three", true, true)

	entries := readZIP(t, f.get("/ul/"+f.view+"/zip").Body.Bytes())
	for _, want := range []string{"Series/day1/img001.jpg", "Series/day2/img002.jpg", "escape.txt"} {
		if entries[want] == nil {
			t.Errorf("ZIP lacks %s; has %v", want, entries)
		}
	}
	for name := range entries {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			t.Errorf("ZIP entry %q escapes the archive", name)
		}
	}
	if cd := f.get("/ul/" + f.view + "/file/a").Header().Get("Content-Disposition"); cd != buildContentDisposition("img001.jpg") {
		t.Errorf("single download named %q, want the file name without its folder", cd)
	}
	if page := f.get("/ul/" + f.view + "/files").Body.String(); !strings.Contains(page, ">img001.jpg<") || !strings.Contains(page, "Series/day1/") {
		t.Error("file page does not show the file name with its folder")
	}
}
