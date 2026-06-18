package handler

import (
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"time"

	"github.com/your-org/ferri/internal/store"
	"github.com/your-org/ferri/static"
	"github.com/your-org/ferri/web"
)

var templates *template.Template

// displayLocation is the timezone used for formatting dates in templates.
// Defaults to UTC; overridden by InitTemplates at startup.
var displayLocation = time.UTC

// InitTemplates (re-)initialises the template set with the given timezone.
// Call this once in main() after loading config, before the HTTP server starts.
func InitTemplates(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	displayLocation = loc
	buildTemplates()
}

func init() {
	// Default init with UTC so tests that never call InitTemplates still work.
	buildTemplates()
}

// uploadJSVer is a short hash of upload.js, computed once at startup.
// Used as a cache-busting query parameter in templates: /static/upload.js?v={{uploadJSVer}}
var uploadJSVer string

func buildTemplates() {
	if data, err := static.FS.ReadFile("files/upload.js"); err == nil {
		h := fnv.New32a()
		h.Write(data)
		uploadJSVer = fmt.Sprintf("%d", h.Sum32())
	} else {
		uploadJSVer = "0"
	}

	funcMap := template.FuncMap{
		"formatDate": func(t time.Time) string {
			return t.In(displayLocation).Format("2 Jan 2006 15:04")
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
		"uploadJSVer": func() string { return uploadJSVer },
	}

	var err error
	templates, err = template.New("").Funcs(funcMap).ParseFS(web.Templates,
		"templates/*.html",
		"templates/admin/*.html",
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
