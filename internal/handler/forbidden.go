package handler

import (
	"net/http"
	"strings"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/middleware"
)

// Forbidden renders the branded 403 page shown by the IP allowlist.
// It never shows the client IP or network ranges. With remote_login on, it
// has the sign-in form, except under /admin (sign-in does not open it).
func Forbidden(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Content-Type must be set before WriteHeader; renderPage sets it after.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		renderPage(w, "forbidden.html", struct {
			baseData
			RemoteLogin bool
			Email       string
		}{
			baseData: baseData{
				PageTitle: "Not available here",
				Settings:  middleware.GetSettings(r),
			},
			RemoteLogin: cfg.RemoteLoginEnabled() && !strings.HasPrefix(r.URL.Path, "/admin"),
		})
	}
}
