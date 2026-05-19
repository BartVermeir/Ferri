package handler

// Admin handler for Ferri.
//
// Routes (IP-restricted + admin session cookie):
//   GET  /admin/login                    — login form
//   POST /admin/login                    — validate token, set session cookie
//   POST /admin/logout                   — clear session cookie
//   GET  /admin                          — dashboard
//   GET  /admin/transfers                — list all transfers
//   POST /admin/transfers/:id/delete     — soft-delete a transfer
//   GET  /admin/mail                     — mail queue overview
//   POST /admin/mail/:id/retry           — reset failed mail to pending
//   POST /admin/mail/:id/delete          — delete mail from queue
//   GET  /admin/settings                 — runtime settings form
//   POST /admin/settings                 — save settings
//
// Authentication (architecture.md §4 admin authentication):
//   Login: compare submitted token with cfg.Admin.Token using subtle.ConstantTimeCompare.
//   Session: HMAC-SHA256 signed cookie, validated by AdminAuth middleware.
//   On invalid session: redirect to /admin/login (not 403 — hides admin panel existence).

import (
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/your-org/ferri/internal/config"
	appMiddleware "github.com/your-org/ferri/internal/middleware"
	"github.com/your-org/ferri/internal/store"
)

// ── Login / Logout ────────────────────────────────────────────────────────────

// AdminLogin handles GET /admin/login.
func AdminLogin(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderAdminLogin(w, "")
	}
}

// AdminLoginPost handles POST /admin/login.
// Compares the submitted token against cfg.Admin.Token using constant-time compare,
// sets a signed session cookie on success, redirects to /admin on success.
func AdminLoginPost(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			renderAdminLogin(w, "Invalid request.")
			return
		}

		submitted := r.FormValue("token")

		// Constant-time compare — prevents timing attacks that could
		// reveal the length or partial content of the admin token.
		if subtle.ConstantTimeCompare([]byte(submitted), []byte(cfg.Admin.Token)) != 1 {
			// Add a small deliberate delay to further slow brute-force attempts.
			// The IP allowlist is the primary protection; this is defence-in-depth.
			time.Sleep(500 * time.Millisecond)
			renderAdminLogin(w, "Invalid token.")
			return
		}

		appMiddleware.SetAdminCookie(w, cfg.Admin.Token, cfg.SessionTTL())
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// AdminLogout handles POST /admin/logout.
func AdminLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appMiddleware.ClearAdminCookie(w)
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

// AdminDashboard handles GET /admin.
func AdminDashboard(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		// Fetch data for dashboard
		activeTransfers, err := stores.Transfers.ListActive(100)
		if err != nil {
			slog.Error("admin dashboard: list active", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		failedMailCount, err := stores.Mail.CountFailed()
		if err != nil {
			slog.Error("admin dashboard: count failed mail", "error", err)
			failedMailCount = 0 // non-fatal
		}

		renderAdminDashboard(w, settings, activeTransfers, failedMailCount)
	}
}

// ── Transfers ─────────────────────────────────────────────────────────────────

// AdminTransfers handles GET /admin/transfers.
func AdminTransfers(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		transfers, err := stores.Transfers.ListAll(500)
		if err != nil {
			slog.Error("admin transfers: list", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		renderAdminTransfers(w, settings, transfers)
	}
}

// AdminTransferDelete handles POST /admin/transfers/:id/delete.
// Soft-deletes a transfer (status → 'deleted'). Does NOT remove files from disk —
// the cleanup job handles that based on the grace period.
func AdminTransferDelete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing transfer ID", http.StatusBadRequest)
			return
		}

		if err := stores.Transfers.SoftDelete(id); err != nil {
			slog.Error("admin: soft delete transfer", "id", id, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("admin: transfer soft-deleted", "id", id)
		http.Redirect(w, r, "/admin/transfers", http.StatusSeeOther)
	}
}

// ── Mail queue ────────────────────────────────────────────────────────────────

// AdminMail handles GET /admin/mail.
func AdminMail(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		failedMails, err := stores.Mail.ListFailed(200)
		if err != nil {
			slog.Error("admin mail: list failed", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		renderAdminMail(w, settings, failedMails)
	}
}

// AdminMailRetry handles POST /admin/mail/:id/retry.
// Resets a failed mail to pending with attempts = 0.
func AdminMailRetry(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing mail ID", http.StatusBadRequest)
			return
		}

		if err := stores.Mail.Retry(id); err != nil {
			slog.Error("admin: retry mail", "id", id, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("admin: mail queued for retry", "id", id)
		http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
	}
}

// AdminMailDelete handles POST /admin/mail/:id/delete.
func AdminMailDelete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing mail ID", http.StatusBadRequest)
			return
		}

		if err := stores.Mail.Delete(id); err != nil {
			slog.Error("admin: delete mail", "id", id, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("admin: mail deleted", "id", id)
		http.Redirect(w, r, "/admin/mail", http.StatusSeeOther)
	}
}

// ── Settings ──────────────────────────────────────────────────────────────────

// AdminSettings handles GET /admin/settings.
func AdminSettings(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		renderAdminSettings(w, settings, "")
	}
}

// AdminSettingsSave handles POST /admin/settings.
// Saves each setting key individually. Only known keys are accepted.
func AdminSettingsSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, settings, "Invalid form data.")
			return
		}

		// Allowlist of settable keys — prevents arbitrary key injection.
		// Checkbox fields (notify_on_download, expiry_summary) must be handled
		// explicitly: an unchecked checkbox sends NO form value, so r.FormValue
		// returns "". The settings cache evaluates '' != "false" as true, meaning
		// unchecked boxes would be permanently stuck on. We normalise to "true"/"false".
		checkboxVal := func(key string) string {
			if r.FormValue(key) == "true" {
				return "true"
			}
			return "false"
		}

		allowed := map[string]string{
			"branding.company_name":   r.FormValue("branding.company_name"),
			"branding.logo_url":       r.FormValue("branding.logo_url"),
			"branding.primary_color":  r.FormValue("branding.primary_color"),
			"branding.accent_color":   r.FormValue("branding.accent_color"),
			"branding.bg_color":       r.FormValue("branding.bg_color"),
			"ui.welcome_message":      r.FormValue("ui.welcome_message"),
			"ui.send_page_title":      r.FormValue("ui.send_page_title"),
			"ui.download_page_title":  r.FormValue("ui.download_page_title"),
			"mail.from_name":          r.FormValue("mail.from_name"),
			"mail.from_address":       r.FormValue("mail.from_address"),
			"mail.notify_on_download": checkboxVal("mail.notify_on_download"),
			"mail.expiry_summary":     checkboxVal("mail.expiry_summary"),
		}

		// Settings are saved individually. If one fails, earlier saves are not
		// rolled back — a partial update is possible. For independent key-value
		// branding/UI settings this is acceptable; a retry saves all 12 again.
		var saveErr string
		for key, value := range allowed {
			if err := stores.Settings.Save(key, strings.TrimSpace(value)); err != nil {
				slog.Error("admin settings: save", "key", key, "error", err)
				saveErr = fmt.Sprintf("Failed to save setting '%s'. Other settings may have been saved.", key)
				break
			}
		}

		if saveErr != "" {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, settings, saveErr)
			return
		}

		slog.Info("admin: settings saved")
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}
}

// ── Template rendering placeholders ──────────────────────────────────────────


// AdminLogoUpload handles POST /admin/settings/logo
func AdminLogoUpload(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(5 << 20); err != nil {
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}
		file, header, err := r.FormFile("logo")
		if err != nil {
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}
		defer file.Close()

		// Validate extension
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".svg" && ext != ".webp" {
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}

		// Save to storage/logo directory
		logoDir := filepath.Join(cfg.Storage.Path, "logo")
		if err := os.MkdirAll(logoDir, 0755); err != nil {
			slog.Error("logo upload: mkdir", "error", err)
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}

		logoPath := filepath.Join(logoDir, "logo"+ext)
		dst, err := os.Create(logoPath)
		if err != nil {
			slog.Error("logo upload: create file", "error", err)
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}
		defer dst.Close()
		if _, err := io.Copy(dst, file); err != nil {
			slog.Error("logo upload: write file", "error", err)
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}

		// Save logo URL to settings
		logoURL := "/static/logo/logo" + ext
		if err := stores.Settings.Save("branding.logo_url", logoURL); err != nil {
			slog.Error("logo upload: save setting", "error", err)
		}

		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}
}

// AdminLogoDelete handles POST /admin/settings/logo/delete
func AdminLogoDelete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Clear the setting
		if err := stores.Settings.Save("branding.logo_url", ""); err != nil {
			slog.Error("logo delete: save setting", "error", err)
		}
		// Remove files
		logoDir := filepath.Join(cfg.Storage.Path, "logo")
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".svg", ".webp"} {
			os.Remove(filepath.Join(logoDir, "logo"+ext))
		}
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}
}

func renderAdminLogin(w http.ResponseWriter, errMsg string) {
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	// Login page uses minimal inline HTML (no settings available yet)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errHTML := ""
	if errMsg != "" {
		errHTML = `<p style="color:red;margin-bottom:16px">` + errMsg + `</p>`
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Admin login</title><meta charset="utf-8">
<style>*{box-sizing:border-box;margin:0;padding:0}body{font-family:system-ui,sans-serif;background:#f8f8f6;display:flex;align-items:center;justify-content:center;min-height:100vh}.card{background:#fff;border:1px solid #e8e8e4;border-radius:12px;padding:32px;width:340px}h1{font-size:20px;font-weight:500;margin-bottom:24px}label{display:block;font-size:13px;font-weight:500;color:#555;margin-bottom:4px}input{width:100%%;padding:9px 12px;border:1px solid #ddd;border-radius:8px;font-size:14px;outline:none;margin-bottom:16px}.btn{width:100%%;padding:10px;border:none;border-radius:8px;background:#000;color:#fff;font-size:14px;font-weight:500;cursor:pointer}</style>
</head>
<body>
<div class="card">
  <h1>Admin login</h1>
  %s
  <form method="POST" action="/admin/login">
    <label>Token</label>
    <input type="password" name="token" autofocus required>
    <button type="submit" class="btn">Login</button>
  </form>
</div>
</body>
</html>`, errHTML)
}

type dashboardStats struct {
	ActiveTransfers int
	ActiveRequests  int
	PendingMails    int
	FailedMails     int
}

func renderAdminDashboard(w http.ResponseWriter, settings *store.Settings, transfers []store.Transfer, failedMailCount int) {
	active := 0
	for _, t := range transfers {
		if t.Status == "active" {
			active++
		}
	}
	// Show only recent 10
	recent := transfers
	if len(recent) > 10 {
		recent = recent[:10]
	}
	renderPage(w, "admin/dashboard.html", struct {
		adminData
		Stats           dashboardStats
		RecentTransfers []store.Transfer
	}{
		adminData: adminData{PageTitle: "Dashboard", ActiveNav: "dashboard", Settings: settings},
		Stats: dashboardStats{
			ActiveTransfers: active,
			FailedMails:     failedMailCount,
		},
		RecentTransfers: recent,
	})
}

func renderAdminTransfers(w http.ResponseWriter, settings *store.Settings, transfers []store.Transfer) {
	renderPage(w, "admin/transfers.html", struct {
		adminData
		Transfers []store.Transfer
	}{
		adminData: adminData{PageTitle: "Transfers", ActiveNav: "transfers", Settings: settings},
		Transfers: transfers,
	})
}

func renderAdminMail(w http.ResponseWriter, settings *store.Settings, mails []store.MailItem) {
	renderPage(w, "admin/mail.html", struct {
		adminData
		Mails []store.MailItem
	}{
		adminData: adminData{PageTitle: "Mail queue", ActiveNav: "mail", Settings: settings},
		Mails:     mails,
	})
}

func renderAdminSettings(w http.ResponseWriter, settings *store.Settings, errMsg string) {
	saved := errMsg == "saved"
	if saved {
		errMsg = ""
	}
	renderPage(w, "admin/settings.html", struct {
		adminData
		Saved bool
		Error string
	}{
		adminData: adminData{PageTitle: "Settings", ActiveNav: "settings", Settings: settings},
		Saved:     saved,
		Error:     errMsg,
	})
}
