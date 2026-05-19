package handler

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/your-org/ferri/internal/store"
	"github.com/your-org/ferri/web"
)

var templates *template.Template

func init() {
	funcMap := template.FuncMap{
		"formatDate": func(t time.Time) string {
			return t.Format("2 Jan 2006 15:04")
		},
		"formatSize": func(b int64) string {
			const unit = 1024
			if b < unit {
				return fmt.Sprintf("%d B", b)
			}
			div, exp := int64(unit), 0
			for n := b / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
		},
	}

	var err error
	templates, err = template.New("").Funcs(funcMap).ParseFS(
		web.Templates,
		"templates/base.html",
		"templates/send.html",
		"templates/download.html",
		"templates/password.html",
		"templates/upload.html",
		"templates/request.html",
		"templates/request_created.html",
		"templates/upload_complete.html",
		"templates/admin/base.html",
		"templates/admin/dashboard.html",
		"templates/admin/transfers.html",
		"templates/admin/mail.html",
		"templates/admin/settings.html",
	)
	if err != nil {
		panic("failed to parse templates: " + err.Error())
	}
}

// renderPage renders a named template with the given data.
func renderPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// baseData contains fields common to all public pages.
type baseData struct {
	PageTitle string
	Settings  *store.Settings
}

// adminData contains fields common to all admin pages.
type adminData struct {
	PageTitle string
	ActiveNav string
	Settings  *store.Settings
}
