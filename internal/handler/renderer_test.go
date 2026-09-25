package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit L5: a template that fails halfway must not send half a page with the
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
