package static

import (
	"bytes"
	"testing"
)

// TestVendoredAssetsPresent guards against the embedded static assets going
// missing (e.g. a bad //go:embed change or an accidental file move). The TUS
// client is vendored rather than loaded from a CDN — see the CSP in
// internal/middleware/security.go.
func TestVendoredAssetsPresent(t *testing.T) {
	for _, f := range []string{
		"files/tus.min.js", "files/upload.js", "files/favicon.svg",
		"files/base.js", "files/send.js", "files/request-created.js", "files/upload-init.js",
		"files/admin-settings.js", "files/admin-dashboard.js",
	} {
		b, err := FS.ReadFile(f)
		if err != nil {
			t.Fatalf("embedded asset %s missing: %v", f, err)
		}
		if len(b) < 50 {
			t.Errorf("embedded asset %s is suspiciously small (%d bytes)", f, len(b))
		}
	}

	tus, _ := FS.ReadFile("files/tus.min.js")
	if !bytes.Contains(tus, []byte("tus")) {
		t.Error("files/tus.min.js does not look like the tus-js-client build")
	}

	// Folders go as loose files (DEC-035); nothing packs in the browser.
	if _, err := FS.ReadFile("files/client-zip.js"); err == nil {
		t.Error("files/client-zip.js is back, but nothing uses it")
	}

}
