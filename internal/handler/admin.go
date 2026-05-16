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
	"log/slog"
	"net/http"
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
			"mail.notify_on_download": r.FormValue("mail.notify_on_download"),
			"mail.expiry_summary":     r.FormValue("mail.expiry_summary"),
		}

		var saveErr string
		for key, value := range allowed {
			if err := stores.Settings.Save(key, strings.TrimSpace(value)); err != nil {
				slog.Error("admin settings: save", "key", key, "error", err)
				saveErr = fmt.Sprintf("Failed to save setting: %s", key)
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

func renderAdminLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = fmt.Sprintf(`<p style="color:red">%s</p>`, errMsg)
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Admin login</title><meta charset="utf-8"></head>
<body>
  <h1>Admin login</h1>
  %s
  <form method="POST" action="/admin/login">
    <label>Token<br><input type="password" name="token" autofocus required></label><br><br>
    <button type="submit">Login</button>
  </form>
</body>
</html>`, errHTML)
}

func renderAdminDashboard(w http.ResponseWriter, settings *store.Settings, transfers []store.Transfer, failedMailCount int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	mailBadge := ""
	if failedMailCount > 0 {
		mailBadge = fmt.Sprintf(` <span style="color:red">(%d failed)</span>`, failedMailCount)
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Admin — %s</title><meta charset="utf-8"></head>
<body>
  <h1>%s — Admin</h1>
  <nav>
    <a href="/admin">Dashboard</a> |
    <a href="/admin/transfers">Transfers</a> |
    <a href="/admin/mail">Mail queue%s</a> |
    <a href="/admin/settings">Settings</a> |
    <form method="POST" action="/admin/logout" style="display:inline">
      <button type="submit">Logout</button>
    </form>
  </nav>
  <hr>
  <h2>Active transfers (%d)</h2>
  <ul>`,
		settings.CompanyName, settings.CompanyName, mailBadge, len(transfers))

	for _, t := range transfers {
		fmt.Fprintf(w, `<li>%s — %s (expires %s)</li>`,
			t.Title, t.SenderEmail, t.ExpiresAt.Format("2 Jan 2006"))
	}

	fmt.Fprintf(w, `</ul></body></html>`)
}

func renderAdminTransfers(w http.ResponseWriter, settings *store.Settings, transfers []store.Transfer) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Transfers — Admin</title><meta charset="utf-8"></head>
<body>
  <h1>All transfers</h1>
  <a href="/admin">← Dashboard</a>
  <table border="1" cellpadding="4">
    <tr><th>Title</th><th>Sender</th><th>Status</th><th>Expires</th><th>Action</th></tr>`)

	for _, t := range transfers {
		deleteBtn := fmt.Sprintf(`<form method="POST" action="/admin/transfers/%s/delete" style="display:inline">
      <button type="submit" onclick="return confirm('Delete this transfer?')">Delete</button>
    </form>`, t.ID)
		if t.Status == "deleted" {
			deleteBtn = "—"
		}
		fmt.Fprintf(w, `<tr>
      <td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>
    </tr>`,
			t.Title, t.SenderEmail, t.Status,
			t.ExpiresAt.Format("2 Jan 2006 15:04"),
			deleteBtn)
	}

	fmt.Fprintf(w, `</table></body></html>`)
}

func renderAdminMail(w http.ResponseWriter, settings *store.Settings, mails []store.MailItem) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Mail queue — Admin</title><meta charset="utf-8"></head>
<body>
  <h1>Failed mail queue (%d)</h1>
  <a href="/admin">← Dashboard</a>
  <table border="1" cellpadding="4">
    <tr><th>To</th><th>Subject</th><th>Attempts</th><th>Error</th><th>Actions</th></tr>`,
		len(mails))

	for _, m := range mails {
		errMsg := ""
		if m.ErrorMessage.Valid {
			errMsg = m.ErrorMessage.String
		}
		fmt.Fprintf(w, `<tr>
      <td>%s</td><td>%s</td><td>%d/%d</td><td>%s</td>
      <td>
        <form method="POST" action="/admin/mail/%s/retry" style="display:inline">
          <button type="submit">Retry</button>
        </form>
        <form method="POST" action="/admin/mail/%s/delete" style="display:inline">
          <button type="submit" onclick="return confirm('Delete this mail?')">Delete</button>
        </form>
      </td>
    </tr>`,
			m.ToAddress, m.Subject, m.Attempts, m.MaxAttempts, errMsg,
			m.ID, m.ID)
	}

	fmt.Fprintf(w, `</table></body></html>`)
}

func renderAdminSettings(w http.ResponseWriter, settings *store.Settings, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusInternalServerError)
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = fmt.Sprintf(`<p style="color:red">%s</p>`, errMsg)
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Settings — Admin</title><meta charset="utf-8"></head>
<body>
  <h1>Settings</h1>
  <a href="/admin">← Dashboard</a>
  %s
  <form method="POST" action="/admin/settings">
    <h2>Branding</h2>
    <label>Company name<br><input type="text" name="branding.company_name" value="%s"></label><br><br>
    <label>Logo URL<br><input type="text" name="branding.logo_url" value="%s"></label><br><br>
    <label>Primary colour<br><input type="color" name="branding.primary_color" value="%s"></label><br><br>
    <label>Accent colour<br><input type="color" name="branding.accent_color" value="%s"></label><br><br>
    <label>Background colour<br><input type="color" name="branding.bg_color" value="%s"></label><br><br>

    <h2>UI text</h2>
    <label>Welcome message<br><textarea name="ui.welcome_message">%s</textarea></label><br><br>
    <label>Send page title<br><input type="text" name="ui.send_page_title" value="%s"></label><br><br>
    <label>Download page title<br><input type="text" name="ui.download_page_title" value="%s"></label><br><br>

    <h2>Mail</h2>
    <label>From name<br><input type="text" name="mail.from_name" value="%s"></label><br><br>
    <label>From address<br><input type="email" name="mail.from_address" value="%s"></label><br><br>
    <label><input type="checkbox" name="mail.notify_on_download" value="true" %s>
      Notify sender on each download</label><br><br>
    <label><input type="checkbox" name="mail.expiry_summary" value="true" %s>
      Send expiry summary when transfer expires</label><br><br>

    <button type="submit">Save settings</button>
  </form>
</body>
</html>`,
		errHTML,
		settings.CompanyName,
		settings.LogoURL,
		settings.PrimaryColor,
		settings.AccentColor,
		settings.BgColor,
		settings.WelcomeMessage,
		settings.SendPageTitle,
		settings.DownloadPageTitle,
		settings.MailFromName,
		settings.MailFromAddress,
		checkedIf(settings.NotifyOnDownload),
		checkedIf(settings.ExpirySummary),
	)
}

func checkedIf(b bool) string {
	if b {
		return "checked"
	}
	return ""
}
