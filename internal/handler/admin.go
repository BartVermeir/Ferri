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
	"encoding/json"
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
	"github.com/your-org/ferri/internal/storage"
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

// ── Overview (combined dashboard + transfers + requests + mail) ───────────────

// adminOverviewData holds everything the combined overview page needs.
type adminOverviewData struct {
	adminData
	Transfers    []store.TransferSummary
	Requests     []store.RequestSummary
	FailedMails  []store.MailItem
	TotalBytes   int64
	PendingBytes int64
}

// AdminDashboard handles GET /admin — combined overview page.
func AdminDashboard(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		transfers, err := stores.Transfers.ListForAdmin(500)
		if err != nil {
			slog.Error("admin overview: list transfers", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		requests, err := stores.Requests.ListForAdmin(500)
		if err != nil {
			slog.Error("admin overview: list requests", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		failedMails, err := stores.Mail.ListFailed(50)
		if err != nil {
			slog.Error("admin overview: list failed mails", "error", err)
			failedMails = nil // non-fatal
		}

		var totalBytes int64
		for _, t := range transfers {
			totalBytes += t.TotalBytes
		}
		for _, r := range requests {
			totalBytes += r.TotalBytes
		}

		pendingT, _ := stores.Transfers.SumPendingCleanupBytes()
		pendingR, _ := stores.Requests.SumPendingCleanupBytes()
		pendingBytes := pendingT + pendingR

		renderPage(w, "admin/dashboard.html", adminOverviewData{
			adminData:    adminData{PageTitle: "Overview", ActiveNav: "dashboard", Settings: settings},
			Transfers:    transfers,
			Requests:     requests,
			FailedMails:  failedMails,
			TotalBytes:   totalBytes,
			PendingBytes: pendingBytes,
		})
	}
}

// AdminForceCleanup handles POST /admin/cleanup.
// Runs the cleanup job immediately, bypassing the grace period.
func AdminForceCleanup(cfg *config.Config, scheduler interface{ RunCleanupNow() }) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("admin: force cleanup triggered")
		go scheduler.RunCleanupNow() // run in background — page redirects immediately
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}
func AdminTransfers(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// ── Delete (transfer or upload request) ──────────────────────────────────────

// AdminTransferDelete handles POST /admin/transfers/:id/delete.
// Hard-deletes: removes all files from storage, marks records deleted in DB.
func AdminTransferDelete(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing transfer ID", http.StatusBadRequest)
			return
		}

		// Remove files from storage.
		// Files are stored flat by TUS as <tus_upload_id> on the backend.
		// storage_path holds the logical path; TUSUploadID holds the actual filename.
		// Mirror the same fallback logic used in the download handler.
		files, err := stores.Transfers.GetFilesByTransferID(id)
		if err != nil {
			slog.Error("admin: get files for delete", "id", id, "error", err)
		} else {
			for _, f := range files {
				_ = mgr.Remove(f.StoragePath)
				_ = mgr.Remove(f.StoragePath + ".info")
				if f.TUSUploadID.Valid {
					_ = mgr.Remove(f.TUSUploadID.String)
					_ = mgr.Remove(f.TUSUploadID.String + ".info")
				}
			}
		}
		// Best-effort removal of the transfer directory (empty for TUS uploads)
		_ = mgr.RemoveAll("transfers/" + id)

		if err := stores.Transfers.MarkFilesDeleted(id); err != nil {
			slog.Error("admin: mark files deleted", "id", id, "error", err)
		}
		if err := stores.Transfers.SoftDelete(id); err != nil {
			slog.Error("admin: soft delete transfer", "id", id, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("admin: transfer hard-deleted", "id", id)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// AdminRequestDelete handles POST /admin/requests/:id/delete.
// Hard-deletes an upload request and its files from storage.
func AdminRequestDelete(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing request ID", http.StatusBadRequest)
			return
		}

		// Mirror download handler fallback: try storage_path first, then TUSUploadID
		files, err := stores.Requests.GetFiles(id)
		if err != nil {
			slog.Error("admin: get request files for delete", "id", id, "error", err)
		} else {
			for _, f := range files {
				_ = mgr.Remove(f.StoragePath)
				_ = mgr.Remove(f.StoragePath + ".info")
				if f.TUSUploadID.Valid {
					_ = mgr.Remove(f.TUSUploadID.String)
					_ = mgr.Remove(f.TUSUploadID.String + ".info")
				}
			}
		}
		_ = mgr.RemoveAll("requests/" + id)

		if err := stores.Requests.SetExpired(id); err != nil {
			slog.Error("admin: mark request deleted", "id", id, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("admin: upload request hard-deleted", "id", id)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
		storageSaved := r.URL.Query().Get("storage_saved") == "1"
		storageError := r.URL.Query().Get("storage_error")
		renderAdminSettings(w, settings, "", storageSaved, storageError)
	}
}

// AdminSettingsSave handles POST /admin/settings.
// Saves each setting key individually. Only known keys are accepted.
func AdminSettingsSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, settings, "Invalid form data.", false, "")
			return
		}

		// Allowlist of settable keys — prevents arbitrary key injection.
		// Checkbox fields (notify_on_download, expiry_summary) must be handled
		// explicitly: an unchecked checkbox sends NO form value, so r.FormValue
		// returns "". The settings cache evaluates '' != "false" as true, meaning
		// unchecked boxes would be permanently stuck on. We normalise to "true"/"false".
		checkboxVal := func(formKey string) string {
			v := r.FormValue(formKey)
			if v == "1" || v == "true" {
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
			"branding.font_family":    r.FormValue("branding.font_family"),
			"ui.welcome_message":      r.FormValue("branding.welcome_message"),
			"ui.send_page_title":      r.FormValue("branding.send_page_title"),
			"ui.download_page_title":  r.FormValue("branding.download_page_title"),
			"mail.from_name":          r.FormValue("mail.from_name"),
			"mail.from_address":       r.FormValue("mail.from_address"),
			"mail.notify_on_download": checkboxVal("notify.on_download"),
			"mail.expiry_summary":     checkboxVal("notify.expiry_summary"),
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
			renderAdminSettings(w, settings, saveErr, false, "")
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

func renderAdminSettings(w http.ResponseWriter, settings *store.Settings, errMsg string, storageSaved bool, storageError string) {
	saved := errMsg == "saved"
	if saved {
		errMsg = ""
	}
	renderPage(w, "admin/settings.html", struct {
		adminData
		Saved        bool
		Error        string
		StorageSaved bool
		StorageError string
	}{
		adminData:    adminData{PageTitle: "Settings", ActiveNav: "settings", Settings: settings},
		Saved:        saved,
		Error:        errMsg,
		StorageSaved: storageSaved,
		StorageError: storageError,
	})
}

// ── Storage settings ──────────────────────────────────────────────────────────

// AdminStorageSave handles POST /admin/settings/storage.
// Saves SMB storage settings, encrypting the password with the admin token key.
// After saving, reloads the storage Manager so the new backend is used immediately.
func AdminStorageSave(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/admin/settings?storage_error=invalid+form", http.StatusSeeOther)
			return
		}

		storageType := r.FormValue("storage.type")
		if storageType != "local" && storageType != "smb" {
			storageType = "local"
		}

		saves := map[string]string{
			"storage.type":          storageType,
			"storage.smb_host":      strings.TrimSpace(r.FormValue("storage.smb_host")),
			"storage.smb_share":     strings.TrimSpace(r.FormValue("storage.smb_share")),
			"storage.smb_base_path": strings.TrimSpace(r.FormValue("storage.smb_base_path")),
			"storage.smb_username":  strings.TrimSpace(r.FormValue("storage.smb_username")),
			"storage.smb_domain":    strings.TrimSpace(r.FormValue("storage.smb_domain")),
		}

		// Only update password if a new one was provided (empty = keep existing).
		newPassword := r.FormValue("storage.smb_password")
		if newPassword != "" {
			key := storage.DeriveKey(cfg.Admin.Token)
			encrypted, err := storage.Encrypt(key, newPassword)
			if err != nil {
				slog.Error("admin storage: encrypt password", "error", err)
				http.Redirect(w, r, "/admin/settings?storage_error=encrypt+failed", http.StatusSeeOther)
				return
			}
			saves["storage.smb_password_encrypted"] = encrypted
		}

		for key, value := range saves {
			if err := stores.Settings.Save(key, value); err != nil {
				slog.Error("admin storage: save setting", "key", key, "error", err)
				http.Redirect(w, r, "/admin/settings?storage_error=save+failed", http.StatusSeeOther)
				return
			}
		}

		// Reload the storage backend immediately with the new settings.
		settings := stores.Settings.Get()
		newBackend, err := storage.FromSettings(settings, cfg, cfg.Admin.Token)
		if err != nil {
			slog.Error("admin storage: reload backend", "error", err)
			http.Redirect(w, r, "/admin/settings?storage_error="+err.Error(), http.StatusSeeOther)
			return
		}
		mgr.Swap(newBackend)

		slog.Info("admin: storage settings saved", "type", storageType)
		http.Redirect(w, r, "/admin/settings?storage_saved=1", http.StatusSeeOther)
	}
}

// AdminStorageTest handles POST /admin/settings/storage/test.
// Tests the connection to the configured storage backend.
// Returns JSON: {"ok": true} or {"ok": false, "error": "..."}.
func AdminStorageTest(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// r.FormValue handles both url-encoded and multipart automatically.
		host := strings.TrimSpace(r.FormValue("storage.smb_host"))
		share := strings.TrimSpace(r.FormValue("storage.smb_share"))
		basePath := strings.TrimSpace(r.FormValue("storage.smb_base_path"))
		username := strings.TrimSpace(r.FormValue("storage.smb_username"))
		domain := strings.TrimSpace(r.FormValue("storage.smb_domain"))

		// Use submitted password if provided; otherwise decrypt stored one.
		password := r.FormValue("storage.smb_password")
		if password == "" {
			settings := stores.Settings.Get()
			if settings.SMBPasswordEncrypted != "" {
				key := storage.DeriveKey(cfg.Admin.Token)
				decrypted, err := storage.Decrypt(key, settings.SMBPasswordEncrypted)
				if err == nil {
					password = decrypted
				}
			}
		}

		if host == "" || share == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "host and share are required"})
			return
		}

		b, err := storage.NewSMBBackend(storage.SMBConfig{
			Host:     host,
			Share:    share,
			BasePath: basePath,
			Username: username,
			Password: password,
			Domain:   domain,
		})
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer b.Close()

		if err := b.TestConnection(); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}

		writeJSON(w, map[string]any{"ok": true})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

