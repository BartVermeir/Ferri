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
	"fmt"
	"net/url"
	"time"
	"path/filepath"
	"os"
	"io"
	"archive/zip"
	"html"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/your-org/ferri/internal/config"
	appMiddleware "github.com/your-org/ferri/internal/middleware"
	"github.com/your-org/ferri/internal/store"
)

const uploadPasswordCookie = "ferri_ul_auth"

// ── Upload page ───────────────────────────────────────────────────────────────

// UploadPage handles GET /ul/:token.
// If the request is completed, shows the uploaded files instead of the upload form.
func UploadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetByUploadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if req == nil {
			renderUploadNotFound(w, settings)
			return
		}

		if req.PasswordHash.Valid {
			if !uploadPasswordValid(r, tok, req.PasswordHash.String) {
				renderUploadPasswordPage(w, tok, settings, "")
				return
			}
		}

		// If already completed, show the uploaded files
		if req.Status == "completed" {
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
				Files:        files,
				DownloadBase: "/ul/" + tok,
			})
			return
		}

		renderUploadPage(w, tok, req, settings)
	}
}

// UploadPassword handles POST /ul/:token.
func UploadPassword(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
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

		if !req.PasswordHash.Valid {
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

		http.SetCookie(w, &http.Cookie{
			Name:     uploadPasswordCookie + "_" + tok,
			Value:    req.PasswordHash.String,
			Path:     "/ul/" + tok,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})

		http.Redirect(w, r, "/ul/"+tok, http.StatusSeeOther)
	}
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
			if !uploadPasswordValid(r, tok, req.PasswordHash.String) {
				http.Redirect(w, r, "/ul/"+tok, http.StatusSeeOther)
				return
			}
		}

		// Verify at least one file was uploaded before marking complete.
		// Without this, a user who bypasses the JS can mark a request complete
		// with zero files, causing a misleading "files received" notification.
		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			slog.Error("upload complete: get files", "request_id", req.ID, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if len(files) == 0 {
			renderUploadPage(w, tok, req, settings)
			return
		}

		if err := stores.Requests.Complete(req.ID); err != nil {
			slog.Error("upload complete: mark completed", "request_id", req.ID, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("upload request completed", "request_id", req.ID, "token", tok)

		// Enqueue notification mail to requester
		if settings.MailFromAddress != "" {
			subject := fmt.Sprintf("Files received: %s", req.Title)
			bodyHTML := buildUploadCompleteHTML(req, cfg.Server.BaseURL)
			bodyText := buildUploadCompleteText(req, cfg.Server.BaseURL)
			if err := stores.Mail.Enqueue(nil, req.RequesterEmail, subject, bodyHTML, bodyText); err != nil {
				slog.Error("upload complete: enqueue mail", "to", req.RequesterEmail, "error", err)
			}
		}

		// Render thank-you page
		renderUploadComplete(w, req, settings)
	}
}

// ── Password cookie ───────────────────────────────────────────────────────────

func uploadPasswordValid(r *http.Request, tok, bcryptHash string) bool {
	cookie, err := r.Cookie(uploadPasswordCookie + "_" + tok)
	if err != nil {
		return false
	}
	return len(cookie.Value) == len(bcryptHash) &&
		bcryptHashEqual(cookie.Value, bcryptHash)
}

// bcryptHashEqual compares two bcrypt hashes in constant time.
func bcryptHashEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ── Mail body builders ────────────────────────────────────────────────────────

func buildUploadCompleteHTML(req *store.UploadRequest, baseURL string) string {
	viewURL := baseURL + "/ul/" + req.UploadToken + "/files"
	msgPart := ""
	if req.Message != "" {
		msgPart = fmt.Sprintf(`<p style="margin:0 0 16px;font-size:14px;color:#555;">%s</p>`, html.EscapeString(req.Message))
	}
	return fmt.Sprintf(
		`<p style="margin:0 0 16px;font-size:15px;color:#333;">Files have been uploaded for your request <strong>%s</strong>.</p>%s<table width="100%%" cellpadding="0" cellspacing="0" style="margin:24px 0;"><tr><td><a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:8px;font-size:14px;font-weight:500;">View uploaded files</a></td></tr></table>`,
		html.EscapeString(req.Title), msgPart, viewURL,
	)
}

func buildUploadCompleteText(req *store.UploadRequest, baseURL string) string {
	return fmt.Sprintf(
		"Files received for your request '%s'.\n\n%s\n\nView files: %s/ul/%s/files",
		req.Title, req.Message, baseURL, req.UploadToken,
	)
}

// ── Template rendering placeholders ──────────────────────────────────────────


// RequestDownloadPage handles GET /ul/:token/files — shows uploaded files for requester.
func RequestDownloadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		req, err := stores.Requests.GetByUploadTokenAny(tok)
		if err != nil || req == nil {
			renderUploadNotFound(w, settings)
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
			Files:        files,
			DownloadBase: "/ul/" + tok,
		})
	}
}

// RequestDownloadFile handles GET /ul/:token/file/:fileID — stream uploaded file.
func RequestDownloadFile(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		fileID := chi.URLParam(r, "fileID")

		req, err := stores.Requests.GetByUploadTokenAny(tok)
		if err != nil || req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var target *store.UploadRequestFile
		for i := range files {
			if files[i].ID == fileID {
				target = &files[i]
				break
			}
		}
		if target == nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		absPath := filepath.Join(cfg.Storage.Path, target.StoragePath)
		f, err := os.Open(absPath)
		if err != nil && os.IsNotExist(err) && target.TUSUploadID.Valid {
			absPath = filepath.Join(cfg.Storage.Path, target.TUSUploadID.String)
			f, err = os.Open(absPath)
		}
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Disposition", "attachment; filename=" + url.QueryEscape(target.OriginalName))
		http.ServeContent(w, r, target.OriginalName, time.Time{}, f)
	}
}

// RequestDownloadZIP handles GET /ul/:token/zip — stream all uploaded files as ZIP.
func RequestDownloadZIP(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")

		req, err := stores.Requests.GetByUploadTokenAny(tok)
		if err != nil || req == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		files, err := stores.Requests.GetFiles(req.ID)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=" + url.QueryEscape(req.Title) + ".zip")

		zw := zip.NewWriter(w)
		defer zw.Close()

		for _, f := range files {
			absPath := filepath.Join(cfg.Storage.Path, f.StoragePath)
			src, err := os.Open(absPath)
			if err != nil && os.IsNotExist(err) && f.TUSUploadID.Valid {
				absPath = filepath.Join(cfg.Storage.Path, f.TUSUploadID.String)
				src, err = os.Open(absPath)
			}
			if err != nil {
				continue
			}
			entry, err := zw.Create(f.OriginalName)
			if err != nil {
				src.Close()
				continue
			}
			io.Copy(entry, src)
			src.Close()
		}
	}
}

func renderUploadPage(w http.ResponseWriter, tok string, req *store.UploadRequest, settings *store.Settings) {
	renderPage(w, "upload.html", struct {
		baseData
		Request     *store.UploadRequest
		CompleteURL string
	}{
		baseData:    baseData{PageTitle: req.Title, Settings: settings},
		Request:     req,
		CompleteURL: "/ul/" + tok + "/complete",
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

func renderUploadNotFound(w http.ResponseWriter, settings *store.Settings) {
	w.WriteHeader(http.StatusNotFound)
	renderPage(w, "upload_complete.html", struct {
		baseData
		Message string
	}{
		baseData: baseData{PageTitle: "Not found", Settings: settings},
		Message:  "This upload link has expired or does not exist.",
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
