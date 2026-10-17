package handler

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLogoFileServerNoDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := http.StripPrefix("/static/logo/", LogoFileServer(dir))

	// The file itself is served.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/static/logo/logo.png", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "PNGDATA" {
		t.Fatalf("logo.png: code=%d body=%q, want 200 / PNGDATA", rec.Code, rec.Body.String())
	}

	// The directory is not listed.
	for _, p := range []string{"/static/logo/", "/static/logo"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("GET %s returned 200 (directory listing leaked): %q", p, rec.Body.String())
		}
	}
}

// A logo must be an image of the kind its extension says.
func TestAdminLogoUpload_ChecksContent(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	for name, tc := range map[string]struct {
		file    string
		content []byte
		saved   bool
	}{
		"png":         {"logo.png", png, true},
		"html as png": {"logo.png", []byte("<html><script>alert(1)</script></html>"), false},
		"png as jpg":  {"logo.jpg", png, false},
	} {
		cfg := newTestConfig()
		cfg.Storage.Path = t.TempDir()
		stores := newTestStores(t)
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, _ := w.CreateFormFile("logo", tc.file)
		part.Write(tc.content)
		w.Close()
		req := httptest.NewRequest(http.MethodPost, "/admin/settings/logo", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		AdminLogoUpload(cfg, stores).ServeHTTP(httptest.NewRecorder(), req)
		if got := stores.Settings.Get().LogoURL != ""; got != tc.saved {
			t.Errorf("%s: logo saved = %v, want %v", name, got, tc.saved)
		}
	}
}
