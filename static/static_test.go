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
		"files/admin-settings.js", "files/admin-dashboard.js", "files/client-zip.js",
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

	// upload.js imports makeZip and predictLength from this module (DEC-035).
	cz, _ := FS.ReadFile("files/client-zip.js")
	for _, want := range []string{"client-zip 2.5.1", "N as makeZip", "S as predictLength"} {
		if !bytes.Contains(cz, []byte(want)) {
			t.Errorf("files/client-zip.js lacks %q", want)
		}
	}
}
