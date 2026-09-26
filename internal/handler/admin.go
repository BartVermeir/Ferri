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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/relpath"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
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

		appMiddleware.SetAdminCookie(w, cfg.Admin.Token, cfg.SessionTTL(), cfg.Server.SecureCookies)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// AdminLogout handles POST /admin/logout.
//
// Sessions are stateless HMAC cookies (no server-side store), so logout clears
// the browser's cookie but cannot invalidate a cookie value captured elsewhere;
// such a cookie remains valid until its embedded expiry (admin.session_ttl_hours,
// default 8h). To revoke ALL sessions immediately, rotate ADMIN_TOKEN — the
// signing key is derived from it, so every existing cookie fails validation.
// Accepted trade-off: the admin panel is reachable from the internal network
// only, and a server-side session store would add state for little gain.
func AdminLogout(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appMiddleware.ClearAdminCookie(w, cfg.Server.SecureCookies)
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}

// ── Overview (combined dashboard + transfers + requests + mail) ───────────────

// adminOverviewData holds everything the combined overview page needs.
type adminOverviewData struct {
	adminData
	Transfers      []store.TransferSummary
	Requests       []store.RequestSummary
	FailedMails    []store.MailItem
	TotalBytes     int64
	PendingBytes   int64
	OrphansDeleted int // set when redirected back after orphan cleanup
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

		orphans, _ := strconv.Atoi(r.URL.Query().Get("orphans"))

		renderPage(w, "admin/dashboard.html", adminOverviewData{
			adminData:      adminData{PageTitle: "Overview", ActiveNav: "dashboard", Settings: settings},
			Transfers:      transfers,
			Requests:       requests,
			FailedMails:    failedMails,
			TotalBytes:     totalBytes,
			PendingBytes:   pendingBytes,
			OrphansDeleted: orphans,
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

// AdminOrphanClean handles POST /admin/orphans/clean.
// Scans local storage for files not referenced in the DB and deletes them.
// Only works for local storage; returns 400 for SMB.
func AdminOrphanClean(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		if settings.StorageType == "smb" {
			http.Error(w, "Orphan cleanup is only supported for local storage", http.StatusBadRequest)
			return
		}

		storagePath := settings.LocalPath
		if storagePath == "" {
			storagePath = cfg.Storage.Path
		}
		if pathContainsDB(storagePath, cfg.DB.Path) {
			slog.Error("orphan scan refused: storage path contains the database", "path", storagePath, "db", cfg.DB.Path)
			http.Error(w, "Refused: the storage path contains the database.", http.StatusBadRequest)
			return
		}

		known, err := stores.Files.AllTUSUploadIDs()
		if err != nil {
			slog.Error("orphan scan: query tus ids", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		entries, err := os.ReadDir(storagePath)
		if err != nil {
			slog.Error("orphan scan: read dir", "path", storagePath, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var deleted, kept int
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".info") {
				continue
			}
			name := e.Name()
			// Only what looks like a TUS upload is ever a candidate: anything
			// else in the folder (a database, a probe file) is not ours to
			// delete (audit L8).
			if known[name] || !isTUSUploadID(name) {
				continue
			}
			// UUID not in known set. Fall back to the .info file: it may belong to
			// an abandoned upload whose row was never written, or a legacy request
			// file uploaded before tus_upload_id was retained on completion. Keep it
			// if its .info still references a live transfer/request.
			if ref, _ := orphanInfoReferenced(storagePath, name, stores.Files); ref {
				kept++
				continue
			}
			if err := os.Remove(filepath.Join(storagePath, name)); err != nil {
				slog.Warn("orphan clean: remove", "file", name, "error", err)
				kept++
			} else {
				os.Remove(filepath.Join(storagePath, name+".info"))
				deleted++
			}
		}

		slog.Info("orphan cleanup done", "deleted", deleted, "kept_as_referenced", kept)
		http.Redirect(w, r, fmt.Sprintf("/admin?orphans=%d", deleted), http.StatusSeeOther)
	}
}

// orphanInfoReferenced reads the TUS .info file for name and checks whether its
// embedded transfer_id or upload_request_token still exists in the DB.
// Returns false (not referenced) if the .info file is missing or unparseable.
func orphanInfoReferenced(storageRoot, name string, files *store.FilesStore) (bool, error) {
	data, err := os.ReadFile(filepath.Join(storageRoot, name+".info"))
	if err != nil {
		return false, nil
	}
	var info struct {
		MetaData map[string]string `json:"MetaData"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return false, nil
	}
	// The TUS handler replaces the client's metadata with transfer_id or
	// request_id; upload_request_token only occurs in old .info files. Looking
	// for the token alone never matched a request file (audit L8).
	return files.TransferOrRequestExists(
		info.MetaData["transfer_id"],
		info.MetaData["request_id"],
		info.MetaData["upload_request_token"],
	)
}

// tusIDPattern matches the upload IDs our TUS stores create: 32 hex digits
// (tusd's filestore) or a UUID (the SMB store).
var tusIDPattern = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func isTUSUploadID(name string) bool { return tusIDPattern.MatchString(name) }

// pathContainsDB reports whether the database file lies in dir or below it.
// Such a storage path is refused: orphan cleanup would treat the database as
// an unknown file (audit L8).
func pathContainsDB(dir, dbPath string) bool {
	if dir == "" || dbPath == "" || dbPath == ":memory:" {
		return false
	}
	absDir, err1 := filepath.Abs(dir)
	absDB, err2 := filepath.Abs(dbPath)
	if err1 != nil || err2 != nil {
		return true // cannot tell: refuse
	}
	rel, err := filepath.Rel(absDir, filepath.Dir(absDB))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func AdminTransfers(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// ── Delete (transfer or upload request) ──────────────────────────────────────

// AdminTransferDelete handles POST /admin/transfers/:id/delete.
// Hard-deletes: removes all files from storage, marks records deleted in DB.
func AdminTransferDelete(cfg *config.Config, stores *store.Stores, mgr *storage.Manager, summaries deletionSummarizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing transfer ID", http.StatusBadRequest)
			return
		}

		if err := deleteTransferNow(stores, mgr, summaries, id, false); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		slog.Info("admin: transfer hard-deleted", "id", id)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// deleteTransferNow deletes a transfer at once, no grace period: the admin
// delete and the sender's "Delete" on the manage page (bySender) both go
// through here. First the sender's "who downloaded what" summary (the file
// list is empty afterwards; a failure must not stop the delete), then the
// files off storage. A failed removal keeps the file's tus_upload_id, so the
// cleanup job retries it on its next run.
func deleteTransferNow(stores *store.Stores, mgr *storage.Manager, summaries deletionSummarizer, id string, bySender bool) error {
	if sent, err := summaries.EnqueueDeletionSummary(id, bySender); err != nil {
		slog.Error("delete: enqueue deletion summary", "id", id, "error", err)
	} else if sent {
		slog.Info("delete: deletion summary queued", "id", id, "by_sender", bySender)
	}

	files, err := stores.Transfers.GetFilesByTransferID(id)
	if err != nil {
		slog.Error("delete: get files", "id", id, "error", err)
	} else {
		for _, f := range files {
			if err := storage.PurgeUpload(mgr, stores.Files, store.TransferFiles, f.ID, f.StoragePath, f.TUSUploadID.String); err != nil {
				slog.Warn("delete: remove file failed, cleanup job will retry", "file", f.ID, "error", err)
			}
		}
	}
	_ = mgr.RemoveAll("transfers/" + id)

	if err := stores.Transfers.MarkFilesDeleted(id); err != nil {
		slog.Error("delete: mark files deleted", "id", id, "error", err)
	}
	if err := stores.Transfers.SoftDelete(id); err != nil {
		slog.Error("delete: soft delete transfer", "id", id, "error", err)
		return err
	}
	return nil
}

// AdminTransferFiles handles GET /admin/transfers/{id}/files: the transfer's
// complete files, for an admin to look at. Downloads from here go through
// AdminTransferFile and are not recorded. The dashboard used to link each
// recipient's own download link, so an admin checking a transfer showed up
// as that recipient downloading it and mailed the sender (audit L6).
func AdminTransferFiles(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := stores.Transfers.GetByID(chi.URLParam(r, "id"))
		if err != nil || t == nil {
			http.NotFound(w, r)
			return
		}
		files, err := stores.Transfers.GetFilesByTransferID(t.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		var complete []store.File
		for _, f := range files {
			if f.Status == "complete" {
				complete = append(complete, f)
			}
		}
		renderPage(w, "admin/transfer_files.html", struct {
			adminData
			Transfer *store.Transfer
			Files    []store.File
		}{
			adminData: adminData{PageTitle: "Transfer files", ActiveNav: "dashboard", Settings: appMiddleware.GetSettings(r)},
			Transfer:  t,
			Files:     complete,
		})
	}
}

// AdminTransferFile handles GET /admin/transfers/{id}/file/{fileID}: serves
// one file without recording a download or notifying anyone.
func AdminTransferFile(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := stores.Transfers.GetFileByID(chi.URLParam(r, "fileID"))
		if err != nil || f == nil || f.TransferID != chi.URLParam(r, "id") || f.Status != "complete" {
			http.NotFound(w, r)
			return
		}
		src, err := mgr.Open(f.StoragePath)
		if err != nil && f.TUSUploadID.Valid && f.TUSUploadID.String != "" {
			src, err = mgr.Open(f.TUSUploadID.String)
		}
		if err != nil {
			slog.Error("admin: open file", "file_id", f.ID, "error", err)
			http.Error(w, "File not found on storage", http.StatusNotFound)
			return
		}
		defer src.Close()
		w.Header().Set("Content-Disposition", buildContentDisposition(relpath.Base(f.OriginalName)))
		http.ServeContent(w, r, f.OriginalName, time.Time{}, src)
	}
}

// deletionSummarizer is the part of jobs.Scheduler the delete handler needs.
type deletionSummarizer interface {
	EnqueueDeletionSummary(transferID string, bySender bool) (bool, error)
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

		if err := deleteRequestNow(stores, mgr, id); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		slog.Info("admin: upload request hard-deleted", "id", id)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

// deleteRequestNow deletes an upload request and its files at once, for the
// admin and for the requester's manage page. Same as transfers: a failed
// removal is retried by the cleanup job.
func deleteRequestNow(stores *store.Stores, mgr *storage.Manager, id string) error {
	files, err := stores.Requests.GetFiles(id)
	if err != nil {
		slog.Error("delete: get request files", "id", id, "error", err)
	} else {
		for _, f := range files {
			if err := storage.PurgeUpload(mgr, stores.Files, store.RequestFiles, f.ID, f.StoragePath, f.TUSUploadID.String); err != nil {
				slog.Warn("delete: remove file failed, cleanup job will retry", "file", f.ID, "error", err)
			}
		}
	}
	_ = mgr.RemoveAll("requests/" + id)

	if err := stores.Requests.MarkFilesDeleted(id); err != nil {
		slog.Error("delete: mark request files deleted", "id", id, "error", err)
	}
	if err := stores.Requests.SoftDelete(id); err != nil {
		slog.Error("delete: mark request deleted", "id", id, "error", err)
		return err
	}
	return nil
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
		renderAdminSettings(w, cfg, settings, "", storageSaved, storageError)
	}
}

// hexColorRe matches a CSS hex colour (#rgb, #rrggbb, #rrggbbaa).
var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

// allowedFonts is the server-side mirror of the font-family <select> in
// admin/settings.html. Anything outside this set is rejected so a crafted POST
// cannot inject an arbitrary font-family value into the public CSS.
var allowedFonts = map[string]bool{
	"":                                    true,
	"'Georgia', serif":                    true,
	"'Helvetica Neue', Arial, sans-serif": true,
}

// validateBranding checks the free-form branding inputs against strict formats
// (defense-in-depth — these settings are admin-only but land unescaped in
// public CSS / <img src>). Returns "" when acceptable, else a user-facing message.
func validateBranding(vals map[string]string) string {
	for _, key := range []string{"branding.primary_color", "branding.accent_color", "branding.bg_color"} {
		if v := strings.TrimSpace(vals[key]); v != "" && !hexColorRe.MatchString(v) {
			return fmt.Sprintf("Invalid colour for '%s' — must be a hex value like #1a2b3c.", key)
		}
	}
	if !allowedFonts[strings.TrimSpace(vals["branding.font_family"])] {
		return "Invalid font family — choose one of the listed options."
	}
	if l := strings.TrimSpace(vals["branding.logo_url"]); l != "" && !validLogoURL(l) {
		return "Invalid logo URL — use a relative path (/static/...) or an https:// URL."
	}
	return ""
}

// validLogoURL allows a site-relative path (single leading slash, not
// protocol-relative) or an absolute https:// URL with a host.
func validLogoURL(s string) bool {
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// maxAlertRecipients is a sanity limit, like maxRecipients on the send form.
const maxAlertRecipients = 20

// normalizeAlertRecipients turns the Alerts field (addresses separated by
// commas or new lines) into "a@example.com, b@example.com", or returns a
// message for the first invalid address. Empty is valid: no alerts.
func normalizeAlertRecipients(raw string) (string, string) {
	list := parseRecipients(raw)
	if len(list) > maxAlertRecipients {
		return "", fmt.Sprintf("At most %d alert recipients.", maxAlertRecipients)
	}
	for _, a := range list {
		if !isValidEmail(a) {
			return "", "Invalid alert recipient: " + a
		}
	}
	return strings.Join(list, ", "), ""
}

// AdminSettingsSave handles POST /admin/settings.
// Saves each setting key individually. Only known keys are accepted.
func AdminSettingsSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, cfg, settings, "Invalid form data.", false, "")
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

		if msg := validateBranding(allowed); msg != "" {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, cfg, settings, msg, false, "")
			return
		}
		alertRecipients, msg := normalizeAlertRecipients(r.FormValue("alerts.recipients"))
		if msg != "" {
			settings := appMiddleware.GetSettings(r)
			renderAdminSettings(w, cfg, settings, msg, false, "")
			return
		}
		allowed["alerts.recipients"] = alertRecipients

		// Settings are saved individually. If one fails, earlier saves are not
		// rolled back — a partial update is possible. For independent key-value
		// branding/UI settings this is acceptable; a retry saves them all again.
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
			renderAdminSettings(w, cfg, settings, saveErr, false, "")
			return
		}

		slog.Info("admin: settings saved")
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}
}

// ── Template rendering placeholders ──────────────────────────────────────────

// AdminLogoUpload handles POST /admin/settings/logo.
//
// The logo is written to <storage.path>/logo on the LOCAL filesystem via os.*,
// not through the storage.Manager — this is deliberate. The logo is small,
// public branding, not user data, and keeping it local avoids a round-trip to
// the SMB share on every page render. It is served by handler.LogoFileServer.
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
		// SVG is excluded: browsers render SVG as HTML, enabling stored XSS via a malicious logo file.
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
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
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp"} {
			os.Remove(filepath.Join(logoDir, "logo"+ext))
		}
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}
}

func renderAdminLogin(w http.ResponseWriter, errMsg string) {
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	renderPage(w, "admin/login.html", struct{ Error string }{Error: errMsg})
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

func renderAdminSettings(w http.ResponseWriter, cfg *config.Config, settings *store.Settings, errMsg string, storageSaved bool, storageError string) {
	saved := errMsg == "saved"
	if saved {
		errMsg = ""
	}
	renderPage(w, "admin/settings.html", struct {
		adminData
		Cfg          *config.Config
		Saved        bool
		Error        string
		StorageSaved bool
		StorageError string
	}{
		adminData:    adminData{PageTitle: "Settings", ActiveNav: "settings", Settings: settings},
		Cfg:          cfg,
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

		if storageType == "local" {
			localPath := strings.TrimSpace(r.FormValue("storage.local_path"))
			if localPath != "" && !filepath.IsAbs(localPath) {
				http.Redirect(w, r, "/admin/settings?storage_error=local+path+must+be+absolute", http.StatusSeeOther)
				return
			}
			if pathContainsDB(localPath, cfg.DB.Path) {
				http.Redirect(w, r, "/admin/settings?storage_error="+url.QueryEscape("the storage folder must not contain the database ("+cfg.DB.Path+")"), http.StatusSeeOther)
				return
			}
		}

		saves := map[string]string{
			"storage.type":          storageType,
			"storage.local_path":    strings.TrimSpace(r.FormValue("storage.local_path")),
			"storage.smb_host":      strings.TrimSpace(r.FormValue("storage.smb_host")),
			"storage.smb_share":     strings.TrimSpace(r.FormValue("storage.smb_share")),
			"storage.smb_base_path": strings.TrimSpace(r.FormValue("storage.smb_base_path")),
			"storage.smb_username":  strings.TrimSpace(r.FormValue("storage.smb_username")),
			"storage.smb_domain":    strings.TrimSpace(r.FormValue("storage.smb_domain")),
		}

		// Only update password if a new one was provided (empty = keep existing).
		// Keeping it is only allowed for the same server and account: the
		// backend reload below would otherwise log in to a new host with the
		// stored credential (audit M4).
		newPassword := r.FormValue("storage.smb_password")
		if newPassword == "" && storageType == "smb" && !mayReuseSMBPassword(stores.Settings.Get(), r) {
			http.Redirect(w, r, "/admin/settings?storage_error="+url.QueryEscape(msgSMBPasswordAgain), http.StatusSeeOther)
			return
		}
		if newPassword != "" {
			encrypted, err := storage.Encrypt(cfg.Admin.Token, newPassword)
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
			http.Redirect(w, r, "/admin/settings?storage_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		mgr.Swap(newBackend)

		slog.Info("admin: storage settings saved", "type", storageType)
		http.Redirect(w, r, "/admin/settings?storage_saved=1", http.StatusSeeOther)
	}
}

const msgSMBPasswordAgain = "Enter the password again: host, share, username or domain differ from the saved settings."

// mayReuseSMBPassword reports whether an empty password field may fall back
// to the saved password: only when there is none, or when the form's host,
// share, username and domain match the saved ones. Otherwise anyone with an
// admin session could enter their own server and capture the service
// account's NTLM login (audit M4). Host and domain are case-insensitive, like
// DNS and Windows domains.
func mayReuseSMBPassword(saved *store.Settings, r *http.Request) bool {
	if saved.SMBPasswordEncrypted == "" {
		return true
	}
	form := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	return strings.EqualFold(form("storage.smb_host"), saved.SMBHost) &&
		form("storage.smb_share") == saved.SMBShare &&
		form("storage.smb_username") == saved.SMBUsername &&
		strings.EqualFold(form("storage.smb_domain"), saved.SMBDomain)
}

// AdminStorageTest handles POST /admin/settings/storage/test.
// Tests the connection to the configured storage backend.
// Returns JSON: {"ok": true} or {"ok": false, "error": "..."}.
func AdminStorageTest(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// r.FormValue handles both url-encoded and multipart automatically.
		storageType := r.FormValue("storage.type")

		if storageType == "local" {
			path := strings.TrimSpace(r.FormValue("storage.local_path"))
			if path == "" {
				path = cfg.Storage.Path
			}
			if !filepath.IsAbs(path) {
				writeJSON(w, map[string]any{"ok": false, "error": "path must be absolute"})
				return
			}
			b := storage.NewLocalBackend(path)
			defer b.Close()
			if err := b.TestConnection(); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}

		host := strings.TrimSpace(r.FormValue("storage.smb_host"))
		share := strings.TrimSpace(r.FormValue("storage.smb_share"))
		basePath := strings.TrimSpace(r.FormValue("storage.smb_base_path"))
		username := strings.TrimSpace(r.FormValue("storage.smb_username"))
		domain := strings.TrimSpace(r.FormValue("storage.smb_domain"))

		// Use submitted password if provided; otherwise decrypt the stored one,
		// but only for the stored server and account (audit M4).
		password := r.FormValue("storage.smb_password")
		if password == "" && !mayReuseSMBPassword(stores.Settings.Get(), r) {
			writeJSON(w, map[string]any{"ok": false, "error": msgSMBPasswordAgain})
			return
		}
		if password == "" {
			settings := stores.Settings.Get()
			if settings.SMBPasswordEncrypted != "" {
				decrypted, err := storage.Decrypt(cfg.Admin.Token, settings.SMBPasswordEncrypted)
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
