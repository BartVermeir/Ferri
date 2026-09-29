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
//   GET  /admin/settings                 — redirect to /admin/settings/branding
//   GET  /admin/settings/{branding,mail,storage} — the three settings pages
//   POST /admin/settings/{branding,mail} — save that page's settings
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/activity"
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
	Transfers    []store.TransferSummary
	Requests     []store.RequestSummary
	Items        []overviewItem // Transfers and Requests together, newest first
	Expired      []overviewItem // past expiry, files not cleaned up yet, newest expiry first
	GraceHours   int
	FailedMails  []store.MailItem
	TotalBytes   int64
	PendingBytes int64
	Deleted      int // set when redirected back after "Delete selected"
	DeleteFailed int
	Now          activityView
}

// overviewItem is one row of the overview table: a transfer or a request.
type overviewItem struct {
	Transfer *store.TransferSummary
	Request  *store.RequestSummary
}

func (it overviewItem) createdAt() time.Time {
	if it.Transfer != nil {
		return it.Transfer.CreatedAt
	}
	return it.Request.CreatedAt
}

func (it overviewItem) expiresAt() time.Time {
	if it.Transfer != nil {
		return it.Transfer.ExpiresAt
	}
	return it.Request.ExpiresAt
}

// mergeExpired is mergeOverview for the expired section: newest expiry first.
func mergeExpired(transfers []store.TransferSummary, requests []store.RequestSummary) []overviewItem {
	items := mergeOverview(transfers, requests)
	sort.SliceStable(items, func(a, b int) bool { return items[a].expiresAt().After(items[b].expiresAt()) })
	return items
}

// mergeOverview puts transfers and requests in one list, newest first, so a
// request shows up where it was created instead of below every transfer.
func mergeOverview(transfers []store.TransferSummary, requests []store.RequestSummary) []overviewItem {
	items := make([]overviewItem, 0, len(transfers)+len(requests))
	for i := range transfers {
		items = append(items, overviewItem{Transfer: &transfers[i]})
	}
	for i := range requests {
		items = append(items, overviewItem{Request: &requests[i]})
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].createdAt().After(items[b].createdAt()) })
	return items
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

		// Non-fatal: the page is still useful without the expired section.
		expiredT, err := stores.Transfers.ListExpiredForAdmin(500)
		if err != nil {
			slog.Error("admin overview: list expired transfers", "error", err)
		}
		expiredR, err := stores.Requests.ListExpiredForAdmin(500)
		if err != nil {
			slog.Error("admin overview: list expired requests", "error", err)
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

		deleted, _ := strconv.Atoi(r.URL.Query().Get("deleted"))
		deleteFailed, _ := strconv.Atoi(r.URL.Query().Get("failed"))

		renderPage(w, "admin/dashboard.html", adminOverviewData{
			adminData:    adminData{PageTitle: "Overview", ActiveNav: "dashboard", Settings: settings},
			Transfers:    transfers,
			Requests:     requests,
			Items:        mergeOverview(transfers, requests),
			Expired:      mergeExpired(expiredT, expiredR),
			GraceHours:   cfg.Jobs.CleanupGraceHours,
			FailedMails:  failedMails,
			TotalBytes:   totalBytes,
			PendingBytes: pendingBytes,
			Deleted:      deleted,
			DeleteFailed: deleteFailed,
			Now:          buildActivityView(stores, activity.Default.Snapshot()),
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
			// delete.
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
		http.Redirect(w, r, fmt.Sprintf("%s?orphans=%d", storageSettings.path, deleted), http.StatusSeeOther)
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
	// request_id; upload_request_token only occurs in old .info files.
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
// an unknown file.
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
// files off storage, then the rows. A failed removal keeps the file's
// tus_upload_id, so the cleanup job retries it on its next run.
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
	// Out of the database at once, unless a file is left on storage: the
	// cleanup job needs the row to retry, and purges it after that.
	if _, err := stores.Transfers.PurgeDeleted(); err != nil {
		slog.Error("delete: purge transfer", "id", id, "error", err)
	}
	return nil
}

// adminFilesData is the admin files page of one transfer or request.
type adminFilesData struct {
	adminData
	Kind       string // "transfer" or "request"
	Title      string
	From       string
	Status     string
	ExpiresAt  time.Time
	Expired    bool
	GraceHours int
	FileBase   string // download URL of a file, without its ID
	Files      []adminFile
}

type adminFile struct {
	ID   string
	Name string
	Size int64
}

// AdminTransferFiles handles GET /admin/transfers/{id}/files: the transfer's
// complete files, for an admin to look at. Downloads from here go through
// AdminTransferFile and are not recorded, unlike a download through a
// recipient's own link. Also after expiry, as long as the cleanup job has not
// removed the files: only an admin gets them then, nothing links here from outside.
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
		var complete []adminFile
		for _, f := range files {
			if f.Status == "complete" {
				complete = append(complete, adminFile{ID: f.ID, Name: f.OriginalName, Size: f.SizeBytes})
			}
		}
		renderPage(w, "admin/files.html", adminFilesData{
			adminData: adminData{PageTitle: "Transfer files", ActiveNav: "dashboard", Settings: appMiddleware.GetSettings(r)},
			Kind:      "transfer", Title: t.Title, From: t.SenderEmail, Status: t.Status,
			ExpiresAt: t.ExpiresAt, Expired: t.Status == "expired" || !t.ExpiresAt.After(time.Now()), GraceHours: cfg.Jobs.CleanupGraceHours,
			FileBase: "/admin/transfers/" + t.ID + "/file/", Files: complete,
		})
	}
}

// AdminRequestFiles handles GET /admin/requests/{id}/files: the request's
// complete files, as AdminTransferFiles does for a transfer.
func AdminRequestFiles(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := stores.Requests.GetByID(chi.URLParam(r, "id"))
		if err != nil || req == nil {
			http.NotFound(w, r)
			return
		}
		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		var complete []adminFile
		for _, f := range completeRequestFiles(files) {
			complete = append(complete, adminFile{ID: f.ID, Name: f.OriginalName, Size: f.SizeBytes})
		}
		renderPage(w, "admin/files.html", adminFilesData{
			adminData: adminData{PageTitle: "Request files", ActiveNav: "dashboard", Settings: appMiddleware.GetSettings(r)},
			Kind:      "request", Title: req.Title, From: req.RequesterEmail, Status: req.Status,
			ExpiresAt: req.ExpiresAt, Expired: req.Status == "expired" || !req.ExpiresAt.After(time.Now()), GraceHours: cfg.Jobs.CleanupGraceHours,
			FileBase: "/admin/requests/" + req.ID + "/file/", Files: complete,
		})
	}
}

// AdminRequestFile handles GET /admin/requests/{id}/file/{fileID}: serves
// one file of a request to the admin, as AdminTransferFile does.
func AdminRequestFile(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := stores.Requests.GetRequestFileByID(chi.URLParam(r, "fileID"))
		if err != nil || f == nil || f.UploadRequestID != chi.URLParam(r, "id") || f.Status != "complete" {
			http.NotFound(w, r)
			return
		}
		src, err := openStored(mgr, f.StoragePath, f.TUSUploadID)
		if err != nil {
			slog.Error("admin: open request file", "file_id", f.ID, "error", err)
			http.Error(w, "File not found on storage", http.StatusNotFound)
			return
		}
		defer src.Close()
		w.Header().Set("Content-Disposition", buildContentDisposition(relpath.Base(f.OriginalName)))
		info := activity.Info{
			Kind: activity.Download, Item: "request", ItemID: f.UploadRequestID, File: f.OriginalName, Who: "admin",
			IP: appMiddleware.ClientIP(r, cfg.TrustedProxies), Offset: rangeStart(r), Total: f.SizeBytes,
		}
		if req, err := stores.Requests.GetByID(f.UploadRequestID); err == nil && req != nil {
			info.Title = req.Title
		}
		act := activity.Default.Start(info)
		defer act.Done()
		ra := newReadAhead(src)
		defer ra.Close() // runs before src.Close: waits for reads still running
		http.ServeContent(act.Writer(w), r, f.OriginalName, time.Time{}, ra)
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
		src, err := openStored(mgr, f.StoragePath, f.TUSUploadID)
		if err != nil {
			slog.Error("admin: open file", "file_id", f.ID, "error", err)
			http.Error(w, "File not found on storage", http.StatusNotFound)
			return
		}
		defer src.Close()
		w.Header().Set("Content-Disposition", buildContentDisposition(relpath.Base(f.OriginalName)))
		info := activity.Info{
			Kind: activity.Download, Item: "transfer", ItemID: f.TransferID, File: f.OriginalName, Who: "admin",
			IP: appMiddleware.ClientIP(r, cfg.TrustedProxies), Offset: rangeStart(r), Total: f.SizeBytes,
		}
		if t, err := stores.Transfers.GetByID(f.TransferID); err == nil && t != nil {
			info.Title = t.Title
		}
		act := activity.Default.Start(info)
		defer act.Done()
		ra := newReadAhead(src)
		defer ra.Close() // runs before src.Close: waits for reads still running
		http.ServeContent(act.Writer(w), r, f.OriginalName, time.Time{}, ra)
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
	if _, err := stores.Requests.PurgeDeleted(); err != nil {
		slog.Error("delete: purge request", "id", id, "error", err)
	}
	return nil
}

// AdminBulkDelete handles POST /admin/delete: the rows ticked on the
// dashboard, as repeated "transfer" and "request" fields. Each goes through
// the same code as its own Delete button, so a live transfer still mails the
// sender the summary. One that fails is logged and skipped; the rest go on.
func AdminBulkDelete(cfg *config.Config, stores *store.Stores, mgr *storage.Manager, summaries deletionSummarizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		deleted, failed := 0, 0
		for _, id := range uniqueNonEmpty(r.PostForm["transfer"]) {
			if err := deleteTransferNow(stores, mgr, summaries, id, false); err != nil {
				failed++
				continue
			}
			deleted++
		}
		for _, id := range uniqueNonEmpty(r.PostForm["request"]) {
			if err := deleteRequestNow(stores, mgr, id); err != nil {
				failed++
				continue
			}
			deleted++
		}
		slog.Info("admin: bulk delete", "deleted", deleted, "failed", failed)
		http.Redirect(w, r, fmt.Sprintf("/admin?deleted=%d&failed=%d", deleted, failed), http.StatusSeeOther)
	}
}

// uniqueNonEmpty drops empty and repeated values, keeping the order.
func uniqueNonEmpty(vals []string) []string {
	seen := make(map[string]bool, len(vals))
	var out []string
	for _, v := range vals {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ── Mail queue ────────────────────────────────────────────────────────────────

// AdminMail handles GET /admin/mail.
func AdminMail(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		mails, err := stores.Mail.ListRecent(200)
		if err != nil {
			slog.Error("admin mail: list", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		renderAdminMail(w, settings, mails)
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

// The settings are split over three pages under Configure. Each page posts
// only its own fields and saves only those: an unchecked checkbox sends
// nothing, so a page that saved every key would switch the mail checkboxes
// off each time Branding is saved.
type settingsPage struct {
	tmpl  string
	title string
	nav   string // ActiveNav in admin/base.html
	path  string
}

var (
	brandingSettings = settingsPage{"admin/settings_branding.html", "Branding", "settings-branding", "/admin/settings/branding"}
	mailSettings     = settingsPage{"admin/settings_mail.html", "Mail", "settings-mail", "/admin/settings/mail"}
	storageSettings  = settingsPage{"admin/settings_storage.html", "Storage", "settings-storage", "/admin/settings/storage"}
)

// AdminSettings handles GET /admin/settings: it goes to the first of the
// three settings pages.
func AdminSettings() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
	}
}

// AdminBrandingSettings handles GET /admin/settings/branding.
func AdminBrandingSettings(cfg *config.Config) http.HandlerFunc {
	return settingsGet(cfg, brandingSettings)
}

// AdminMailSettings handles GET /admin/settings/mail.
func AdminMailSettings(cfg *config.Config) http.HandlerFunc {
	return settingsGet(cfg, mailSettings)
}

// AdminStorageSettings handles GET /admin/settings/storage.
func AdminStorageSettings(cfg *config.Config) http.HandlerFunc {
	return settingsGet(cfg, storageSettings)
}

// settingsGet renders a settings page with what the redirect after a save
// put in the query: ?saved=1, and for storage ?storage_saved=1,
// ?storage_error=… and ?orphans=N.
func settingsGet(cfg *config.Config, page settingsPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		orphans, _ := strconv.Atoi(q.Get("orphans"))
		renderSettingsPage(w, cfg, appMiddleware.GetSettings(r), page, settingsView{
			Saved:          q.Get("saved") == "1",
			StorageSaved:   q.Get("storage_saved") == "1",
			StorageError:   q.Get("storage_error"),
			OrphansDeleted: orphans,
			OrphansRan:     q.Has("orphans"),
		})
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

// AdminBrandingSave handles POST /admin/settings/branding: branding and the
// page texts. Only known keys are accepted.
func AdminBrandingSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			renderSettingsError(w, r, cfg, brandingSettings, "Invalid form data.")
			return
		}

		// The page-text fields are named branding.* in the form, ui.* in the
		// database.
		vals := map[string]string{
			"branding.company_name":  r.FormValue("branding.company_name"),
			"branding.logo_url":      r.FormValue("branding.logo_url"),
			"branding.primary_color": r.FormValue("branding.primary_color"),
			"branding.accent_color":  r.FormValue("branding.accent_color"),
			"branding.bg_color":      r.FormValue("branding.bg_color"),
			"branding.font_family":   r.FormValue("branding.font_family"),
			"ui.welcome_message":     r.FormValue("branding.welcome_message"),
			"ui.send_page_title":     r.FormValue("branding.send_page_title"),
			"ui.download_page_title": r.FormValue("branding.download_page_title"),
		}
		if msg := validateBranding(vals); msg != "" {
			renderSettingsError(w, r, cfg, brandingSettings, msg)
			return
		}
		saveSettings(w, r, cfg, stores, brandingSettings, vals)
	}
}

// AdminMailSettingsSave handles POST /admin/settings/mail: sender,
// notifications and alert recipients. Only known keys are accepted.
func AdminMailSettingsSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			renderSettingsError(w, r, cfg, mailSettings, "Invalid form data.")
			return
		}

		// An unchecked checkbox sends no value, so FormValue returns "". The
		// settings cache reads '' != "false" as true, meaning an unchecked
		// box would stay on. Normalise to "true"/"false".
		checkboxVal := func(formKey string) string {
			v := r.FormValue(formKey)
			if v == "1" || v == "true" {
				return "true"
			}
			return "false"
		}

		alertRecipients, msg := normalizeAlertRecipients(r.FormValue("alerts.recipients"))
		if msg != "" {
			renderSettingsError(w, r, cfg, mailSettings, msg)
			return
		}
		saveSettings(w, r, cfg, stores, mailSettings, map[string]string{
			"mail.from_name":          r.FormValue("mail.from_name"),
			"mail.from_address":       r.FormValue("mail.from_address"),
			"mail.notify_on_download": checkboxVal("notify.on_download"),
			"mail.expiry_summary":     checkboxVal("notify.expiry_summary"),
			"alerts.recipients":       alertRecipients,
		})
	}
}

// saveSettings saves each key and goes back to the page with "saved". If one
// fails, earlier saves are not rolled back — a partial update is possible.
// For independent key-value settings this is acceptable; a retry saves them
// all again.
func saveSettings(w http.ResponseWriter, r *http.Request, cfg *config.Config, stores *store.Stores, page settingsPage, vals map[string]string) {
	for key, value := range vals {
		if err := stores.Settings.Save(key, strings.TrimSpace(value)); err != nil {
			slog.Error("admin settings: save", "key", key, "error", err)
			renderSettingsError(w, r, cfg, page, fmt.Sprintf("Failed to save setting '%s'. Other settings may have been saved.", key))
			return
		}
	}
	slog.Info("admin: settings saved", "page", page.title)
	http.Redirect(w, r, page.path+"?saved=1", http.StatusSeeOther)
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
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}
		file, header, err := r.FormFile("logo")
		if err != nil {
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}
		defer file.Close()

		// Validate extension
		ext := strings.ToLower(filepath.Ext(header.Filename))
		// SVG is excluded: browsers render SVG as HTML, enabling stored XSS via a malicious logo file.
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}

		// Save to storage/logo directory
		logoDir := filepath.Join(cfg.Storage.Path, "logo")
		if err := os.MkdirAll(logoDir, 0755); err != nil {
			slog.Error("logo upload: mkdir", "error", err)
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}

		logoPath := filepath.Join(logoDir, "logo"+ext)
		dst, err := os.Create(logoPath)
		if err != nil {
			slog.Error("logo upload: create file", "error", err)
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}
		defer dst.Close()
		if _, err := io.Copy(dst, file); err != nil {
			slog.Error("logo upload: write file", "error", err)
			http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
			return
		}

		// Save logo URL to settings
		logoURL := "/static/logo/logo" + ext
		if err := stores.Settings.Save("branding.logo_url", logoURL); err != nil {
			slog.Error("logo upload: save setting", "error", err)
		}

		http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
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
		http.Redirect(w, r, brandingSettings.path, http.StatusSeeOther)
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

// settingsView is what a settings page shows besides the settings.
type settingsView struct {
	Saved          bool
	Error          string
	StorageSaved   bool
	StorageError   string
	OrphansDeleted int
	OrphansRan     bool
}

func renderSettingsPage(w http.ResponseWriter, cfg *config.Config, settings *store.Settings, page settingsPage, v settingsView) {
	renderPage(w, page.tmpl, struct {
		adminData
		settingsView
		Cfg *config.Config
	}{
		adminData:    adminData{PageTitle: page.title, ActiveNav: page.nav, Settings: settings},
		settingsView: v,
		Cfg:          cfg,
	})
}

func renderSettingsError(w http.ResponseWriter, r *http.Request, cfg *config.Config, page settingsPage, msg string) {
	renderSettingsPage(w, cfg, appMiddleware.GetSettings(r), page, settingsView{Error: msg})
}

// ── Storage settings ──────────────────────────────────────────────────────────

// AdminStorageSave handles POST /admin/settings/storage.
// Saves SMB storage settings, encrypting the password with the admin token key.
// After saving, reloads the storage Manager so the new backend is used immediately.
func AdminStorageSave(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/admin/settings/storage?storage_error=invalid+form", http.StatusSeeOther)
			return
		}

		storageType := r.FormValue("storage.type")
		if storageType != "local" && storageType != "smb" {
			storageType = "local"
		}

		if storageType == "local" {
			localPath := strings.TrimSpace(r.FormValue("storage.local_path"))
			if localPath != "" && !filepath.IsAbs(localPath) {
				http.Redirect(w, r, "/admin/settings/storage?storage_error=local+path+must+be+absolute", http.StatusSeeOther)
				return
			}
			if pathContainsDB(localPath, cfg.DB.Path) {
				http.Redirect(w, r, "/admin/settings/storage?storage_error="+url.QueryEscape("the storage folder must not contain the database ("+cfg.DB.Path+")"), http.StatusSeeOther)
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
		// stored credential.
		newPassword := r.FormValue("storage.smb_password")
		if newPassword == "" && storageType == "smb" && !mayReuseSMBPassword(stores.Settings.Get(), r) {
			http.Redirect(w, r, "/admin/settings/storage?storage_error="+url.QueryEscape(msgSMBPasswordAgain), http.StatusSeeOther)
			return
		}
		if newPassword != "" {
			encrypted, err := storage.Encrypt(cfg.Admin.Token, newPassword)
			if err != nil {
				slog.Error("admin storage: encrypt password", "error", err)
				http.Redirect(w, r, "/admin/settings/storage?storage_error=encrypt+failed", http.StatusSeeOther)
				return
			}
			saves["storage.smb_password_encrypted"] = encrypted
		}

		for key, value := range saves {
			if err := stores.Settings.Save(key, value); err != nil {
				slog.Error("admin storage: save setting", "key", key, "error", err)
				http.Redirect(w, r, "/admin/settings/storage?storage_error=save+failed", http.StatusSeeOther)
				return
			}
		}

		// Reload the storage backend immediately with the new settings.
		settings := stores.Settings.Get()
		newBackend, err := storage.FromSettings(settings, cfg, cfg.Admin.Token)
		if err != nil {
			slog.Error("admin storage: reload backend", "error", err)
			http.Redirect(w, r, "/admin/settings/storage?storage_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		mgr.Swap(newBackend)

		slog.Info("admin: storage settings saved", "type", storageType)
		http.Redirect(w, r, "/admin/settings/storage?storage_saved=1", http.StatusSeeOther)
	}
}

const msgSMBPasswordAgain = "Enter the password again: host, share, username or domain differ from the saved settings."

// mayReuseSMBPassword reports whether an empty password field may fall back
// to the saved password: only when there is none, or when the form's host,
// share, username and domain match the saved ones. Otherwise anyone with an
// admin session could enter their own server and capture the service
// account's NTLM login. Host and domain are case-insensitive, like
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
		// but only for the stored server and account.
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
