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
			bodyHTML := buildUploadCompleteHTML(req)
			bodyText := buildUploadCompleteText(req)
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

func buildUploadCompleteHTML(req *store.UploadRequest) string {
	return fmt.Sprintf(
		`<p>An external party has finished uploading files for your request: <strong>%s</strong>.</p>
<p>%s</p>`,
		req.Title, req.Message,
	)
}

func buildUploadCompleteText(req *store.UploadRequest) string {
	return fmt.Sprintf(
		"Files have been uploaded for your request '%s'.\n\n%s",
		req.Title, req.Message,
	)
}

// ── Template rendering placeholders ──────────────────────────────────────────

func renderUploadPage(w http.ResponseWriter, tok string, req *store.UploadRequest, settings *store.Settings) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Upload files — %s</title><meta charset="utf-8"></head>
<body>
  <h1>%s</h1>
  <p>%s</p>
  <div id="upload-area">
    <input type="file" id="file-input" multiple>
    <div id="file-list"></div>
    <button id="upload-btn">Start upload</button>
  </div>
  <form id="complete-form" method="POST" action="/ul/%s/complete" style="display:none">
    <button type="submit">I'm done uploading</button>
  </form>
  <script>
    window.FERRI_UPLOAD_TOKEN = %q;
  </script>
  <script src="/static/upload.js"></script>
</body>
</html>`,
		settings.CompanyName,
		req.Title,
		req.Message,
		tok,
		tok,
	)
}

func renderUploadPasswordPage(w http.ResponseWriter, tok string, settings *store.Settings, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Password required</title><meta charset="utf-8"></head>
<body>
  <h1>Password required</h1>
  %s
  <form method="POST" action="/ul/%s">
    <input type="password" name="password" autofocus>
    <button type="submit">Continue</button>
  </form>
</body>
</html>`, errMsg, tok)
}

func renderUploadComplete(w http.ResponseWriter, req *store.UploadRequest, settings *store.Settings) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Upload complete</title><meta charset="utf-8"></head>
<body>
  <h1>Thank you</h1>
  <p>Your files have been received. You can close this window.</p>
</body>
</html>`)
}

func renderUploadNotFound(w http.ResponseWriter, settings *store.Settings) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>Not found</title></head>
<body><h1>Upload link not found or expired.</h1></body></html>`)
}
