package handler

// Upload request handler — external party side.
//
// Routes (public — no IP restriction):
//   GET  /ul/:token          — render upload page or password prompt
//   POST /ul/:token          — validate password, set session cookie, redirect
//   POST /ul/:token/complete — external party signals they are done uploading
//
// Flow (architecture.md §5b):
//   External party receives a link /ul/:token from the requester.
//   They open the page, optionally enter a password, then upload files via TUS
//   (JS sends X-Upload-Request-Token in TUS metadata).
//   When done they click "Done" → POST /ul/:token/complete.
//   The handler marks the request completed and enqueues a notification mail.

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

const uploadPasswordCookie = "ferri_ul_auth"

// ── Upload page ───────────────────────────────────────────────────────────────

// UploadPage handles GET /ul/:token.
// A completed request shows the uploader the thank-you page again, never the
// received files: those are for the requester's view link only (audit M1).
func UploadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetLiveByUploadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if req == nil {
			renderUploadNotFound(w, settings)
			return
		}
		if req.Status == "completed" {
			renderUploadComplete(w, req, settings)
			return
		}

		if req.PasswordHash.Valid {
			if !uploadPasswordValid(cfg, r, tok, req.PasswordHash.String) {
				renderUploadPasswordPage(w, tok, settings, "")
				return
			}
		}

		renderUploadPage(w, cfg, tok, req, settings)
	}
}

// UploadPassword handles POST /ul/:token.
func UploadPassword(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetLiveByUploadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		// No password, or the upload was finished meanwhile: the upload page
		// shows the form or the thank-you page.
		if !req.PasswordHash.Valid || req.Status == "completed" {
			http.Redirect(w, r, "/ul/"+tok, http.StatusSeeOther)
			return
		}

		submitted := r.FormValue("password")
		if err := bcrypt.CompareHashAndPassword(
			[]byte(req.PasswordHash.String), []byte(submitted),
		); err != nil {
			renderUploadPasswordPage(w, tok, settings, "Incorrect password.")
			return
		}

		setUploadPasswordCookie(w, cfg, tok, req.PasswordHash.String)
		http.Redirect(w, r, "/ul/"+tok, http.StatusSeeOther)
	}
}

// RequestFilesPassword handles POST /ul/:token/files — the password form shown
// on the requester's file listing posts back to that URL. Unlike UploadPassword
// it takes the view token (GetViewableByViewToken), and it sets the cookie for
// that token's path so the listing, files and ZIP open.
func RequestFilesPassword(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetViewableByViewToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if req == nil {
			renderUploadNotFound(w, settings)
			return
		}

		if !req.PasswordHash.Valid {
			http.Redirect(w, r, "/ul/"+tok+"/files", http.StatusSeeOther)
			return
		}

		submitted := r.FormValue("password")
		if err := bcrypt.CompareHashAndPassword(
			[]byte(req.PasswordHash.String), []byte(submitted),
		); err != nil {
			renderUploadPasswordPage(w, tok, settings, "Incorrect password.")
			return
		}

		setUploadPasswordCookie(w, cfg, tok, req.PasswordHash.String)
		http.Redirect(w, r, "/ul/"+tok+"/files", http.StatusSeeOther)
	}
}

// setUploadPasswordCookie unlocks every /ul/:token/* route (upload page, file
// listing, single file, ZIP) for the browser session.
func setUploadPasswordCookie(w http.ResponseWriter, cfg *config.Config, tok, bcryptHash string) {
	http.SetCookie(w, &http.Cookie{
		Name:     uploadPasswordCookie + "_" + tok,
		Value:    passwordCookieValue(cfg, "ul", tok, bcryptHash, time.Now()),
		Path:     "/ul/" + tok,
		HttpOnly: true,
		Secure:   cfg.Server.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

// ── Upload complete ───────────────────────────────────────────────────────────

// UploadComplete handles POST /ul/:token/complete.
// The external party signals they have finished uploading all files.
func UploadComplete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetByUploadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		// Password check — must have authenticated before completing
		if req.PasswordHash.Valid {
			if !uploadPasswordValid(cfg, r, tok, req.PasswordHash.String) {
				http.Redirect(w, r, "/ul/"+tok, http.StatusSeeOther)
				return
			}
		}

		// Verify at least one file was uploaded before marking complete.
		// Without this, a user who bypasses the JS can mark a request complete
		// with zero files, causing a misleading "files received" notification.
		files, err := waitForRequestFiles(r.Context(), stores.Requests, req.ID)
		if err != nil {
			slog.Error("upload complete: get files", "request_id", req.ID, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if len(files) == 0 {
			renderUploadPage(w, cfg, tok, req, settings)
			return
		}

		if err := stores.Requests.Complete(req.ID); err != nil {
			slog.Error("upload complete: mark completed", "request_id", req.ID, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Do not log the token — it is a bearer secret granting file access.
		slog.Info("upload request completed", "request_id", req.ID)

		// Enqueue notification mail to requester
		if settings.MailFromAddress != "" {
			var items []mail.FileItem
			for _, f := range files {
				if f.Status == "complete" {
					items = append(items, mail.FileItem{Name: f.OriginalName, Size: f.SizeBytes})
				}
			}
			u := uploadCompleteMail{
				Settings: settings,
				BaseURL:  cfg.Server.BaseURL,
				Loc:      cfg.Server.Location,
				Request:  req,
				Files:    items,
			}
			subject := fmt.Sprintf("Received: %s for %s", mail.Plural(len(items), "file"), u.requestLabel())
			bodyHTML := buildUploadCompleteHTML(u)
			bodyText := buildUploadCompleteText(u)
			if err := stores.Mail.Enqueue(nil, req.RequesterEmail, subject, bodyHTML, bodyText); err != nil {
				slog.Error("upload complete: enqueue mail", "to", req.RequesterEmail, "error", err)
			}
		}

		// Render thank-you page
		renderUploadComplete(w, req, settings)
	}
}

// requestFilesSettleTimeout bounds how long UploadComplete waits for the TUS
// completion hook to mark the request's files complete. A variable so tests
// can shorten it.
var requestFilesSettleTimeout = 5 * time.Second

const requestFilesSettlePoll = 50 * time.Millisecond

// waitForRequestFiles returns the request's files once none is still
// 'uploading', or whatever is there when the timeout runs out.
//
// Why wait at all: upload.js submits /complete as soon as the last PATCH is
// answered, but tusd hands the completion event to our hook goroutine before
// that answer and the hook marks the file complete in parallel. When the
// browser wins that race, the last file is still 'uploading' here and would be
// left out of the "files received" mail. Normally the hook needs milliseconds.
// A stray 'uploading' row (an attempt the uploader abandoned) never settles;
// then this costs the full timeout once and the mail lists the complete files.
func waitForRequestFiles(ctx context.Context, requests *store.RequestStore, requestID string) ([]store.UploadRequestFile, error) {
	deadline := time.Now().Add(requestFilesSettleTimeout)
	for {
		files, err := requests.GetFiles(requestID)
		if err != nil {
			return nil, err
		}
		pending := 0
		for _, f := range files {
			if f.Status != "complete" {
				pending++
			}
		}
		if pending == 0 {
			return files, nil
		}
		if time.Now().After(deadline) {
			slog.Warn("upload complete: files still uploading after wait, mailing the complete ones",
				"request_id", requestID, "still_uploading", pending, "waited", requestFilesSettleTimeout)
			return files, nil
		}
		select {
		case <-ctx.Done():
			return files, nil
		case <-time.After(requestFilesSettlePoll):
		}
	}
}

// ── Password cookie ───────────────────────────────────────────────────────────

// uploadPasswordValid checks the signed password cookie (passcookie.go) for
// this link and the request's current password.
func uploadPasswordValid(cfg *config.Config, r *http.Request, tok, bcryptHash string) bool {
	cookie, err := r.Cookie(uploadPasswordCookie + "_" + tok)
	if err != nil {
		return false
	}
	return passwordCookieValid(cfg, "ul", tok, bcryptHash, cookie.Value, time.Now())
}

// ── Mail body builders ────────────────────────────────────────────────────────

// uploadCompleteMail holds what the "files received" mail shows.
type uploadCompleteMail struct {
	Settings *store.Settings
	BaseURL  string
	Loc      *time.Location
	Request  *store.UploadRequest
	Files    []mail.FileItem
}

func (u uploadCompleteMail) viewURL() string {
	// The view token, not the upload token: the upload link went to external
	// parties and must not open the received files (audit M1).
	return u.BaseURL + "/ul/" + u.Request.ViewPathToken() + "/files"
}

func (u uploadCompleteMail) requestLabel() string {
	if u.Request.Title != "" {
		return `"` + u.Request.Title + `"`
	}
	return "your request"
}

// filesWere is "1 file was" / "3 files were".
func (u uploadCompleteMail) filesWere() string {
	if len(u.Files) == 1 {
		return "1 file was"
	}
	return mail.Plural(len(u.Files), "file") + " were"
}

func (u uploadCompleteMail) note() string {
	return "You are receiving this email because you requested files with " + mail.CompanyName(u.Settings) + " and the uploader marked the upload as complete."
}

func buildUploadCompleteHTML(u uploadCompleteMail) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">Hello %s,</p>`, html.EscapeString(u.Request.RequesterName))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s uploaded for %s. You can view and download them now.</p>`,
		html.EscapeString(u.filesWere()), html.EscapeString(u.requestLabel()))
	b.WriteString(mail.QuoteHTML(u.Request.Message))
	b.WriteString(mail.FileListHTML(u.Files))
	b.WriteString(mail.ButtonHTML(u.viewURL(), "View uploaded files", u.Settings))
	fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">The files are available until <strong>%s</strong>. After that date the link stops working.</p>`,
		html.EscapeString(mail.FormatDate(u.Request.ExpiresAt, u.Loc)))
	b.WriteString(mail.NoteHTML(u.note()))

	preheader := fmt.Sprintf("%s received for %s.", mail.Plural(len(u.Files), "file"), u.requestLabel())
	return mail.Wrap(u.Settings, u.BaseURL, preheader, b.String())
}

func buildUploadCompleteText(u uploadCompleteMail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hello %s,\n\n%s uploaded for %s. You can view and download them now.\n\n",
		u.Request.RequesterName, u.filesWere(), u.requestLabel())
	if u.Request.Message != "" {
		b.WriteString(u.Request.Message + "\n\n")
	}
	if len(u.Files) > 0 {
		b.WriteString("Files:\n" + mail.FileListText(u.Files) + "\n")
	}
	fmt.Fprintf(&b, "View files: %s\n\nThe files are available until %s. After that date the link stops working.\n\n--\n%s\n",
		u.viewURL(), mail.FormatDate(u.Request.ExpiresAt, u.Loc), u.note())
	return b.String()
}

// ── Template rendering placeholders ──────────────────────────────────────────

// RequestDownloadPage handles GET /ul/:token/files — shows uploaded files for requester.
func RequestDownloadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetViewableByViewToken(tok)
		if err != nil || req == nil {
			renderUploadNotFound(w, settings)
			return
		}

		// A password-protected request gates file access too — the token alone
		// must not reveal uploaded files. Match the upload-page password check.
		if req.PasswordHash.Valid && !uploadPasswordValid(cfg, r, tok, req.PasswordHash.String) {
			renderUploadPasswordPage(w, tok, settings, "")
			return
		}

		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		renderPage(w, "request_download.html", struct {
			baseData
			Request      *store.UploadRequest
			Files        []store.UploadRequestFile
			DownloadBase string
		}{
			baseData:     baseData{PageTitle: req.Title, Settings: settings},
			Request:      req,
			Files:        completeRequestFiles(files),
			DownloadBase: "/ul/" + tok,
		})
	}
}

// RequestDownloadFile handles GET /ul/:token/file/:fileID — stream uploaded file.
func RequestDownloadFile(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		fileID := chi.URLParam(r, "fileID")

		req, err := stores.Requests.GetViewableByViewToken(tok)
		if err != nil || req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		// Enforce the request password before serving any file bytes.
		if req.PasswordHash.Valid && !uploadPasswordValid(cfg, r, tok, req.PasswordHash.String) {
			http.Redirect(w, r, "/ul/"+tok+"/files", http.StatusSeeOther)
			return
		}

		// Read file fresh from DB to get latest tus_upload_id
		target, err := stores.Requests.GetRequestFileByID(fileID)
		if err != nil || target == nil || target.UploadRequestID != req.ID || target.Status != "complete" {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		f, err := mgr.Open(target.StoragePath)
		if err != nil && target.TUSUploadID.Valid && target.TUSUploadID.String != "" {
			f, err = mgr.Open(target.TUSUploadID.String)
		}
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Disposition", buildContentDisposition(target.OriginalName))
		http.ServeContent(w, r, target.OriginalName, time.Time{}, f)
	}
}

// RequestDownloadZIP handles GET /ul/:token/zip — stream all uploaded files as ZIP.
func RequestDownloadZIP(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")

		req, err := stores.Requests.GetViewableByViewToken(tok)
		if err != nil || req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		// Enforce the request password before serving the ZIP.
		if req.PasswordHash.Valid && !uploadPasswordValid(cfg, r, tok, req.PasswordHash.String) {
			http.Redirect(w, r, "/ul/"+tok+"/files", http.StatusSeeOther)
			return
		}

		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", buildContentDisposition(zipFileName(req.Title)))

		var items []zipItem
		for _, f := range completeRequestFiles(files) {
			items = append(items, zipItem{ID: f.ID, Name: f.OriginalName, StoragePath: f.StoragePath, TUSUploadID: f.TUSUploadID})
		}
		streamZIP(w, mgr, items, "request zip")
	}
}

// completeRequestFiles keeps the files that finished uploading. A row stays
// 'uploading' while a file is still coming in, and for good when that upload
// broke off (until the stalled cleanup); the requester must not get its
// partial data as if it were the file (audit M9).
func completeRequestFiles(files []store.UploadRequestFile) []store.UploadRequestFile {
	var out []store.UploadRequestFile
	for _, f := range files {
		if f.Status == "complete" {
			out = append(out, f)
		}
	}
	return out
}

func renderUploadPage(w http.ResponseWriter, cfg *config.Config, tok string, req *store.UploadRequest, settings *store.Settings) {
	renderPage(w, "upload.html", struct {
		baseData
		Request        *store.UploadRequest
		CompleteURL    string
		MaxFiles       int   // above this, or with a folder, upload.js packs one ZIP (DEC-035)
		MaxUploadBytes int64 // per upload, the ZIP included
	}{
		baseData:       baseData{PageTitle: req.Title, Settings: settings},
		Request:        req,
		CompleteURL:    "/ul/" + tok + "/complete",
		MaxFiles:       cfg.Limits.MaxFilesPerTransfer,
		MaxUploadBytes: cfg.Limits.MaxUploadBytes,
	})
}

func renderUploadPasswordPage(w http.ResponseWriter, tok string, settings *store.Settings, errMsg string) {
	renderPage(w, "password.html", struct {
		baseData
		Token string
		Error string
	}{
		baseData: baseData{PageTitle: "Password required", Settings: settings},
		Token:    tok,
		Error:    errMsg,
	})
}

// renderUploadNotFound shares not_found.html with renderNotFound. It used to
// render upload_complete.html, which ignores .Message and told visitors of a
// dead link "Your files have been received".
func renderUploadNotFound(w http.ResponseWriter, settings *store.Settings) {
	// Headers MUST be set before WriteHeader — after WriteHeader they are ignored.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	renderPage(w, "not_found.html", struct {
		baseData
		Message string
	}{
		baseData: baseData{PageTitle: "Link not available", Settings: settings},
		Message:  "This upload link has expired or does not exist. Ask the person who sent it for a new link.",
	})
}

func renderUploadComplete(w http.ResponseWriter, req *store.UploadRequest, settings *store.Settings) {
	renderPage(w, "upload_complete.html", struct {
		baseData
		Message string
	}{
		baseData: baseData{PageTitle: "Upload complete", Settings: settings},
		Message:  "Your files have been received.",
	})
}
