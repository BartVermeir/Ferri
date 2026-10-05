package handler

import (
	"net/http"

	"github.com/BartVermeir/Ferri/internal/middleware"
)

// Forbidden renders the branded 403 page shown by the IP allowlist.
// It is identical for every restricted route and never shows the client IP or
// network ranges.
func Forbidden() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Content-Type must be set before WriteHeader; renderPage sets it after.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		renderPage(w, "forbidden.html", baseData{
			PageTitle: "Not available here",
			Settings:  middleware.GetSettings(r),
		})
	}
}
