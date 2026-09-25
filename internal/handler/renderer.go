package handler

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/BartVermeir/Ferri/internal/store"
	"github.com/BartVermeir/Ferri/static"
	"github.com/BartVermeir/Ferri/web"
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

// appVersion is set once at startup via SetVersion, from the binary's
// build-time version (see cmd/server/main.go). Defaults to "dev" for
// `go run`/local builds that skip -ldflags.
var appVersion = "dev"

// SetVersion records the build-time version string for display in admin
// templates. Call once during startup, before serving requests.
func SetVersion(v string) {
	if v != "" {
		appVersion = v
	}
}

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
		"appVersion":  func() string { return appVersion },
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

// renderPage renders a named template with the given data. It renders into a
// buffer first: a template that fails halfway used to leave half a page with
// "Template error: <Go error>" under it, sent with the status already
// written (audit L5). Now the visitor gets a plain 500 and the error goes to
// the log.
func renderPage(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
		http.Error(w, "Something went wrong on our side. Please try again later.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
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
