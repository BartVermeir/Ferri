package handler

import (
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
