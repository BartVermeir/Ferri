package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/store"
)

// A template that fails halfway must not send half a page with the
// Go error text under it.
func TestRenderPage_FailureShowsNoTemplateError(t *testing.T) {
	rr := httptest.NewRecorder()
	renderPage(rr, "download.html", 42) // wrong data: fails once the template reads a field

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "Template error") || strings.Contains(body, "can't evaluate") || strings.Contains(body, "<html") {
		t.Fatalf("failed render leaked into the response:\n%s", body)
	}
}

// The chosen font reaches the page CSS; anything else gives the default.
func TestRenderPage_FontFamily(t *testing.T) {
	for font, want := range map[string]string{
		"'Georgia', serif":           "--font: 'Georgia', serif;",
		"":                           "--font: " + defaultFontFamily + ";",
		"x; } body { display:none }": "--font: " + defaultFontFamily + ";",
	} {
		rr := httptest.NewRecorder()
		renderNotFound(rr, &store.Settings{FontFamily: font})
		body := rr.Body.String()
		if !strings.Contains(body, want) || strings.Contains(body, "ZgotmplZ") {
			t.Errorf("font %q: page CSS lacks %q", font, want)
		}
	}
}
