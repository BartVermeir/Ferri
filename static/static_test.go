package static

import (
	"bytes"
	"testing"
)

// TestVendoredAssetsPresent checks that the embedded static assets are present,
// including the vendored TUS client (the CSP in internal/middleware/security.go
// allows no external script host).
func TestVendoredAssetsPresent(t *testing.T) {
	for _, f := range []string{
		"files/tus.min.js", "files/upload.js", "files/favicon.svg",
		"files/base.js", "files/send.js", "files/request-created.js", "files/upload-init.js",
		"files/admin-settings.js", "files/admin-dashboard.js", "files/admin-stats.js",
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

	// client-zip.js is not embedded: folders upload as loose files.
	if _, err := FS.ReadFile("files/client-zip.js"); err == nil {
		t.Error("files/client-zip.js is back, but nothing uses it")
	}

}
