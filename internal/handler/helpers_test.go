package handler

import (
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/config"
)

func TestValidateBranding(t *testing.T) {
	tests := []struct {
		name  string
		vals  map[string]string
		wantOK bool
	}{
		{"empty map", map[string]string{}, true},
		{"valid hex colours + known font", map[string]string{
			"branding.primary_color": "#1a2b3c",
			"branding.accent_color":  "#fff",
			"branding.bg_color":      "#ffffffff",
			"branding.font_family":   "'Georgia', serif",
		}, true},
		{"colour without hash", map[string]string{"branding.primary_color": "red"}, false},
		{"colour with injection payload", map[string]string{"branding.primary_color": "#000;}body{display:none"}, false},
		{"unknown font", map[string]string{"branding.font_family": "Comic Sans MS"}, false},
		{"logo relative path ok", map[string]string{"branding.logo_url": "/static/logo/logo.png"}, true},
		{"logo https ok", map[string]string{"branding.logo_url": "https://cdn.example.com/l.png"}, true},
		{"logo protocol-relative rejected", map[string]string{"branding.logo_url": "//evil.example/l.png"}, false},
		{"logo javascript scheme rejected", map[string]string{"branding.logo_url": "javascript:alert(1)"}, false},
		{"logo http rejected", map[string]string{"branding.logo_url": "http://cdn.example.com/l.png"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := validateBranding(tt.vals)
			if (msg == "") != tt.wantOK {
				t.Fatalf("validateBranding(%v) = %q, wantOK=%v", tt.vals, msg, tt.wantOK)
			}
		})
	}
}

func TestIsValidEmail(t *testing.T) {
	// isValidEmail is deliberately minimal (see its doc comment): it only checks
	// for one '@' with a non-empty local part and a dotted domain. Malformed
	// addresses like "a@@b.com" pass here and are rejected by the SMTP relay
	// at send time — that is by design, not a bug.
	ok := []string{"a@b.co", "first.last@sub.example.com", "x+tag@example.org"}
	bad := []string{"", "no-at", "@example.com", "user@", "user@localhost", "a@b"}
	for _, e := range ok {
		if !isValidEmail(e) {
			t.Errorf("isValidEmail(%q) = false, want true", e)
		}
	}
	for _, e := range bad {
		if isValidEmail(e) {
			t.Errorf("isValidEmail(%q) = true, want false", e)
		}
	}
}

func TestIsValidExpiryOption(t *testing.T) {
	opts := []config.ExpiryOption{{Label: "1 day", Hours: 24}, {Label: "1 week", Hours: 168}}
	if !isValidExpiryOption(24, opts) {
		t.Error("24h should be a valid option")
	}
	if isValidExpiryOption(25, opts) {
		t.Error("25h is not a configured option")
	}
	if isValidExpiryOption(0, nil) {
		t.Error("nothing is valid against an empty option list")
	}
}

func TestUniqueZipName(t *testing.T) {
	seen := map[string]int{}
	got := []string{
		uniqueZipName(seen, "report.pdf"),
		uniqueZipName(seen, "sub/report.pdf"),
		uniqueZipName(seen, "other/report.pdf"),
		uniqueZipName(seen, "notes.txt"),
	}
	want := []string{"report.pdf", "report (1).pdf", "report (2).pdf", "notes.txt"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSanitiseASCIIFilename(t *testing.T) {
	tests := map[string]string{
		"simple.mov":          "simple.mov",
		"Séquence finale.mov": "S_quence finale.mov",
		`bad"name\with/slash`: "bad_name_with_slash",
		"":                    "download",
	}
	for in, want := range tests {
		if got := sanitiseASCIIFilename(in); got != want {
			t.Errorf("sanitiseASCIIFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildContentDisposition(t *testing.T) {
	got := buildContentDisposition("Séquence finale.mov")
	if !strings.HasPrefix(got, "attachment; ") {
		t.Fatalf("missing attachment disposition: %q", got)
	}
	if !strings.Contains(got, `filename="S_quence finale.mov"`) {
		t.Errorf("missing sanitised ASCII fallback in %q", got)
	}
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("missing RFC 5987 filename* in %q", got)
	}
}
