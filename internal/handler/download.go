package handler

// Download handler for Ferri.
//
// Routes:
//   GET  /dl/:token          — render download page (file list)
//   POST /dl/:token          — validate password, set session cookie, redirect
//   GET  /dl/:token/file/:fileID — stream file with Range support
//
// Key points from architecture.md §7:
//   - http.ServeContent for Range/206 support — never io.Copy
//   - Content-Disposition MUST be set before ServeContent (it doesn't set it)
//   - RFC 5987 filename encoding for non-ASCII filenames
//   - Download event recorded in a single transaction with recipient counter update
//   - Download notification mail enqueued (not sent directly) if enabled

import (
	"archive/zip"
	"crypto/subtle"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

// zipCopyBufSize matches the SMB2/3 payload ceiling so ZIP assembly reads
// from SMB-backed storage in large chunks instead of io.Copy's default 32KB
// (which would cap every SMB read below what the negotiated dialect allows).
const zipCopyBufSize = 1 << 20 // 1MB

// downloadPasswordCookie is a short-lived cookie that unlocks a password-protected
// download page for the duration of the browser session. It contains the bcrypt
// hash of the transfer's password so we can validate it without a DB lookup on
// every file request. The cookie is HttpOnly and Secure; it is not signed because
// the bcrypt hash itself is the secret — knowing the hash gives no advantage
// over knowing the password.
const downloadPasswordCookie = "ferri_dl_auth"

// downloadNotifyWindow: at most one download notification per recipient and
// transfer within this window, whatever the number of files. Every counted
// download is still recorded for the expiry summary.
const downloadNotifyWindow = time.Hour

// countsAsDownload reports whether a file request starts a download, as
// opposed to continuing one. A request without Range, or with a Range starting
// at byte 0, starts one; a Range starting later is a resume or a parallel
// chunk of a download already counted. Without this, a download manager or a
// resumed 400 GB download counted (and mailed) once per request.
func countsAsDownload(r *http.Request) bool {
	h := strings.TrimSpace(r.Header.Get("Range"))
	return h == "" || strings.HasPrefix(h, "bytes=0-")
}

// ── Download page ─────────────────────────────────────────────────────────────

// DownloadPage handles GET /dl/:token.
// Validates the token, checks password if set, renders the file list.
func DownloadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		transfer, recipient, files, err := stores.Transfers.GetByDownloadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if transfer == nil {
			renderNotFound(w, settings)
			return
		}

		// Password check — render password form directly rather than redirecting.
		// Redirecting to ?auth=1 and then checking the query param creates a loop:
		// DownloadPage → redirect → DownloadPage → redirect → ...
		if transfer.PasswordHash.Valid {
			if !downloadPasswordValid(r, transfer.PasswordHash.String) {
				renderPasswordPage(w, tok, settings, "")
				return
			}
		}

		// Render download page
		data := downloadPageData{
			Settings:  settings,
			Transfer:  transfer,
			Recipient: recipient,
			Files:     files,
			Token:     tok,
		}
		renderDownloadPage(w, data)
	}
}

// DownloadPassword handles POST /dl/:token.
// Validates the submitted password, sets a session cookie, redirects back.
func DownloadPassword(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")

		transfer, _, _, err := stores.Transfers.GetByDownloadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if transfer == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		if !transfer.PasswordHash.Valid {
			// No password set — redirect to download page directly
			http.Redirect(w, r, "/dl/"+tok, http.StatusSeeOther)
			return
		}

		submitted := r.FormValue("password")
		if err := bcrypt.CompareHashAndPassword(
			[]byte(transfer.PasswordHash.String), []byte(submitted),
		); err != nil {
			// Wrong password — re-render with error
			settings := appMiddleware.GetSettings(r)
			renderPasswordPage(w, tok, settings, "Incorrect password.")
			return
		}

		// Set a session cookie containing the bcrypt hash.
		// The hash is the secret: possessing it proves you know the password.
		// The cookie expires with the browser session (no MaxAge).
		http.SetCookie(w, &http.Cookie{
			Name:     downloadPasswordCookie + "_" + tok,
			Value:    transfer.PasswordHash.String,
			Path:     "/dl/" + tok,
			HttpOnly: true,
			Secure:   cfg.Server.SecureCookies,
			SameSite: http.SameSiteStrictMode,
		})

		http.Redirect(w, r, "/dl/"+tok, http.StatusSeeOther)
	}
}

// ── File download ─────────────────────────────────────────────────────────────

// DownloadFile handles GET /dl/:token/file/:fileID.
// Validates token + file ownership, records the download event, streams the file.
func DownloadFile(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		fileID := chi.URLParam(r, "fileID")
		settings := appMiddleware.GetSettings(r)

		transfer, recipient, files, err := stores.Transfers.GetByDownloadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if transfer == nil {
			renderNotFound(w, settings)
			return
		}

		// Password check — redirect to download page which will render the password form.
		if transfer.PasswordHash.Valid {
			if !downloadPasswordValid(r, transfer.PasswordHash.String) {
				http.Redirect(w, r, "/dl/"+tok, http.StatusSeeOther)
				return
			}
		}

		// Find the requested file among this transfer's files
		var targetFile *store.File
		for i := range files {
			if files[i].ID == fileID {
				targetFile = &files[i]
				break
			}
		}
		if targetFile == nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// Open file from storage.
		// Try our own path layout first (transfers/<transfer_id>/<file_id>),
		// then fall back to the flat tusd layout (<tus_upload_id>).
		f, err := mgr.Open(targetFile.StoragePath)
		if err != nil && targetFile.TUSUploadID.Valid {
			f, err = mgr.Open(targetFile.TUSUploadID.String)
		}
		// Local-only legacy fallback: scan .info files (pre-bugfix uploads).
		if err != nil && mgr.Type() == "local" {
			if found := findFileInStorage(cfg.Storage.Path, targetFile.ID); found != "" {
				rel, _ := filepath.Rel(cfg.Storage.Path, found)
				f, err = mgr.Open(rel)
			}
		}
		if err != nil {
			slog.Error("download: open file", "file_id", fileID, "error", err)
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		defer f.Close()

		// Record download event — in a transaction with recipient counter update.
		// Do this before streaming so the event is recorded even if the client
		// disconnects mid-download. Use the trusted-proxy-aware client IP so a
		// direct client cannot forge the audit-log source via X-Real-IP.
		// Resumes and later chunks (Range not starting at 0) are not a new download.
		notify := false
		if countsAsDownload(r) {
			ip := appMiddleware.ClientIP(r, cfg.TrustedProxies)
			ua := r.Header.Get("User-Agent")

			_, recent, err := stores.Downloads.RecordDownload(
				recipient.ID, targetFile.ID, targetFile.OriginalName, ip, ua, downloadNotifyWindow,
			)
			if err != nil {
				// Log but continue — a recording failure should not block the download
				logDownloadError("record download event", err, fileID)
			}
			notify = err == nil && !recent
		}

		// Enqueue the download notification if enabled, at most once per
		// recipient per downloadNotifyWindow. Not for the sender's own link:
		// telling the sender they downloaded their own file is noise.
		if notify && settings.NotifyOnDownload && settings.MailFromAddress != "" && !recipient.IsSender {
			n := downloadNotice(cfg, settings, transfer, recipient, targetFile.OriginalName)
			subject := fmt.Sprintf("Downloaded: %s", targetFile.OriginalName)
			bodyHTML := buildDownloadNotifyHTML(n)
			bodyText := buildDownloadNotifyText(n)
			if err := stores.Mail.Enqueue(nil, transfer.SenderEmail, subject, bodyHTML, bodyText); err != nil {
				logDownloadError("enqueue download notification", err, fileID)
			}
		}

		// Set Content-Disposition BEFORE calling http.ServeContent.
		// http.ServeContent does NOT set this header — it only sets Content-Type.
		// Without an explicit Content-Disposition: attachment, browsers may render
		// the file inline instead of saving it. This is especially wrong for
		// video files, PDFs, and images.
		w.Header().Set("Content-Disposition", buildContentDisposition(targetFile.OriginalName))

		// Determine modTime for conditional request validation.
		// Use ActivatedAt if set; fall back to now() so http.ServeContent still works.
		modTime := time.Now()
		if transfer.ActivatedAt.Valid {
			modTime = time.Unix(transfer.ActivatedAt.Int64, 0)
		}

		// http.ServeContent handles:
		//   - Range requests (HTTP 206 Partial Content)
		//   - If-Range and If-Modified-Since conditional requests
		//   - Content-Range response headers
		//   - Full response (HTTP 200) when no Range header is present
		// This is critical for 400-600 GB files on flaky connections.
		http.ServeContent(w, r, targetFile.OriginalName, modTime, f)
	}
}

// ── Content-Disposition ───────────────────────────────────────────────────────

// buildContentDisposition constructs a Content-Disposition header value with:
//   - A sanitised ASCII fallback in the legacy `filename` parameter
//   - The full original name RFC 5987 percent-encoded in `filename*`
//
// Example output:
//
//	attachment; filename="Sequence_finale.mov"; filename*=UTF-8''S%C3%A9quence%20finale.mov
//
// Modern browsers use `filename*`; older browsers fall back to `filename`.
// This is required for media filenames that routinely contain
// non-ASCII characters and special characters.
func buildContentDisposition(originalName string) string {
	ascii := sanitiseASCIIFilename(originalName)
	encoded := rfc5987Encode(originalName)
	return fmt.Sprintf(`attachment; filename=%q; filename*=UTF-8''%s`, ascii, encoded)
}

// sanitiseASCIIFilename replaces non-ASCII and special characters with underscores
// to produce a safe ASCII fallback for the legacy `filename` parameter.
func sanitiseASCIIFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r > unicode.MaxASCII || r == '"' || r == '\\' || r == '/' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "download"
	}
	return s
}

// rfc5987Encode percent-encodes a filename per RFC 5987.
// Unreserved characters (letters, digits, a small set of symbols) pass through;
// everything else is percent-encoded as UTF-8 bytes.
func rfc5987Encode(s string) string {
	// RFC 5987 attr-char: ALPHA / DIGIT / "!" / "#" / "$" / "&" / "+" / "-" / "." / "^" / "_" / "`" / "|" / "~"
	return url.PathEscape(s)
}

// ── Password cookie ───────────────────────────────────────────────────────────

// downloadPasswordValid checks whether the request has a valid password cookie
// for the given bcrypt hash. Uses subtle.ConstantTimeCompare to prevent
// timing attacks on the cookie value comparison.
func downloadPasswordValid(r *http.Request, bcryptHash string) bool {
	cookieName := downloadPasswordCookie + "_" + chi.URLParam(r, "token")
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	// Compare the stored bcrypt hash (from the cookie) with the transfer's bcrypt hash.
	// Both are bcrypt hashes — they must be identical (same round of hashing).
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(bcryptHash)) == 1
}

// ── Template rendering placeholders ──────────────────────────────────────────
// Replace with real template rendering when web/templates/ are implemented.

type downloadPageData struct {
	Settings  *store.Settings
	Transfer  *store.Transfer
	Recipient *store.Recipient
	Files     []store.File
	Token     string
}

func renderDownloadPage(w http.ResponseWriter, data downloadPageData) {
	pageTitle := data.Settings.DownloadPageTitle
	if pageTitle == "" {
		pageTitle = "Download files"
	}
	renderPage(w, "download.html", struct {
		baseData
		Transfer     *store.Transfer
		Files        []store.File
		DownloadBase string
	}{
		baseData:     baseData{PageTitle: pageTitle, Settings: data.Settings},
		Transfer:     data.Transfer,
		Files:        data.Files,
		DownloadBase: "/dl/" + data.Token,
	})
}

func renderPasswordPage(w http.ResponseWriter, tok string, settings *store.Settings, errMsg string) {
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

// renderNotFound is shown for unknown and expired download links alike, so the
// page reveals nothing about whether a token ever existed.
func renderNotFound(w http.ResponseWriter, settings *store.Settings) {
	// Headers MUST be set before WriteHeader — after WriteHeader they are ignored.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	renderPage(w, "not_found.html", struct {
		baseData
		Message string
	}{
		baseData: baseData{PageTitle: "Link not available", Settings: settings},
		Message:  "This download link has expired or does not exist. Ask the sender for a new link.",
	})
}

// ── Mail body builders ────────────────────────────────────────────────────────

// downloadNoticeData holds what a download notification shows.
type downloadNoticeData struct {
	Settings *store.Settings
	BaseURL  string
	Loc      *time.Location
	Transfer *store.Transfer
	// Who is the recipient's email, or "" for a link-only transfer: there
	// the one link is shared by the sender, so the address on the row is the
	// sender's own and says nothing about who actually downloaded.
	Who  string
	What string
	At   time.Time
}

func downloadNotice(cfg *config.Config, settings *store.Settings, t *store.Transfer, r *store.Recipient, what string) downloadNoticeData {
	n := downloadNoticeData{
		Settings: settings,
		BaseURL:  cfg.Server.BaseURL,
		Loc:      cfg.Server.Location,
		Transfer: t,
		What:     what,
		At:       time.Now(),
	}
	if t.NotifyRecipients {
		n.Who = r.Email
	}
	return n
}

// transferLabel is the quoted title, or a neutral fallback for an untitled transfer.
func transferLabel(t *store.Transfer) string {
	if t.Title != "" {
		return `"` + t.Title + `"`
	}
	return "your transfer"
}

func (n downloadNoticeData) who() string {
	if n.Who == "" {
		return "Someone with your shared link"
	}
	return n.Who
}

// laterNote explains why the mail names one file: further downloads from the
// same transfer within downloadNotifyWindow are not mailed. It points to the
// expiry summary only when that mail is switched on.
func (n downloadNoticeData) laterNote() string {
	note := "Further downloads from this transfer in the next hour are not mailed separately."
	if n.Settings != nil && n.Settings.ExpirySummary {
		note += " The summary you get when the transfer expires lists every download."
	}
	return note
}

func buildDownloadNotifyHTML(n downloadNoticeData) string {
	// Recipient email, filename (client TUS metadata) and title are all
	// attacker-influenced, so they MUST be HTML-escaped before interpolation.
	var b strings.Builder
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">Hello %s,</p>`, html.EscapeString(n.Transfer.SenderName))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;"><strong>%s</strong> downloaded <strong>%s</strong> from %s.</p>`,
		html.EscapeString(n.who()), html.EscapeString(n.What), html.EscapeString(transferLabel(n.Transfer)))
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 20px;font-size:13px;color:#555;">
  <tr><td style="padding:3px 16px 3px 0;color:#888;">Downloaded</td><td style="padding:3px 0;">%s</td></tr>
  <tr><td style="padding:3px 16px 3px 0;color:#888;">Available until</td><td style="padding:3px 0;">%s</td></tr>
</table>`,
		html.EscapeString(mail.FormatDate(n.At, n.Loc)), html.EscapeString(mail.FormatDate(n.Transfer.ExpiresAt, n.Loc)))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;font-size:13px;color:#555;">%s</p>`, html.EscapeString(n.laterNote()))
	b.WriteString(mail.NoteHTML("You are receiving this email because download notifications are switched on for transfers you send with " + mail.CompanyName(n.Settings) + "."))

	preheader := fmt.Sprintf("%s downloaded %s.", n.who(), n.What)
	return mail.Wrap(n.Settings, n.BaseURL, preheader, b.String())
}

func buildDownloadNotifyText(n downloadNoticeData) string {
	return fmt.Sprintf("Hello %s,\n\n%s downloaded %s from %s.\n\nDownloaded:      %s\nAvailable until: %s\n\n%s\n\n--\nYou are receiving this email because download notifications are switched on for transfers you send with %s.\n",
		n.Transfer.SenderName, n.who(), n.What, transferLabel(n.Transfer),
		mail.FormatDate(n.At, n.Loc), mail.FormatDate(n.Transfer.ExpiresAt, n.Loc),
		n.laterNote(), mail.CompanyName(n.Settings))
}

// ── Logging helper ────────────────────────────────────────────────────────────


// DownloadZIP handles GET /dl/:token/zip.
// Streams all files in the transfer as a ZIP archive.
func DownloadZIP(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)

		transfer, recipient, files, err := stores.Transfers.GetByDownloadToken(tok)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if transfer == nil {
			renderNotFound(w, settings)
			return
		}

		if transfer.PasswordHash.Valid {
			if !downloadPasswordValid(r, transfer.PasswordHash.String) {
				http.Redirect(w, r, "/dl/"+tok, http.StatusSeeOther)
				return
			}
		}

		// Record a download event for each file in the ZIP — before streaming
		// so events are captured even if the client disconnects mid-download.
		// The ZIP is streamed without Range support, so every GET is a download.
		// Same rule as a single file: notify only if this recipient downloaded
		// nothing from the transfer within the window. Only the first event
		// decides; the ones after it see this ZIP's own events.
		ip := appMiddleware.ClientIP(r, cfg.TrustedProxies)
		ua := r.Header.Get("User-Agent")

		notify := false
		for i, f := range files {
			_, recent, err := stores.Downloads.RecordDownload(
				recipient.ID, f.ID, f.OriginalName, ip, ua, downloadNotifyWindow,
			)
			if err != nil {
				logDownloadError("zip: record download event", err, f.ID)
			}
			if i == 0 {
				notify = err == nil && !recent
			}
		}

		// Enqueue a single notification mail for the ZIP download if enabled
		if notify && settings.NotifyOnDownload && settings.MailFromAddress != "" && !recipient.IsSender {
			n := downloadNotice(cfg, settings, transfer, recipient, "all files (ZIP)")
			subject := fmt.Sprintf("Downloaded: all files of %s", transferLabel(transfer))
			bodyHTML := buildDownloadNotifyHTML(n)
			bodyText := buildDownloadNotifyText(n)
			if err := stores.Mail.Enqueue(nil, transfer.SenderEmail, subject, bodyHTML, bodyText); err != nil {
				slog.Error("zip: enqueue download notification", "error", err)
			}
		}

		// Derive a safe ZIP filename from the transfer title
		zipName := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == ' ' {
				return r
			}
			return '_'
		}, transfer.Title)
		if zipName == "" {
			zipName = "files"
		}
		zipName = strings.ReplaceAll(zipName, " ", "_") + ".zip"

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename="+url.QueryEscape(zipName))

		zw := zip.NewWriter(w)
		defer zw.Close()

		seen := map[string]int{}
		buf := make([]byte, zipCopyBufSize)
		for _, f := range files {
			src, err := mgr.Open(f.StoragePath)
			if err != nil && f.TUSUploadID.Valid {
				src, err = mgr.Open(f.TUSUploadID.String)
			}
			if err != nil {
				slog.Error("zip: open file", "file_id", f.ID, "error", err)
				continue
			}

			entry, err := zw.Create(uniqueZipName(seen, f.OriginalName))
			if err != nil {
				src.Close()
				slog.Error("zip: create entry", "file_id", f.ID, "error", err)
				continue
			}
			if _, err := io.CopyBuffer(entry, src, buf); err != nil {
				src.Close()
				slog.Error("zip: copy file", "file_id", f.ID, "error", err)
				continue
			}
			src.Close()
		}
	}
}

func logDownloadError(op string, err error, fileID string) {
	slog.Error("download handler: "+op, "file_id", fileID, "error", err)
}

// uniqueZipName returns a ZIP entry name that is unique within the archive,
// appending " (1)", " (2)", … before the extension when a base name repeats.
// Without this, two files sharing a base name (e.g. two "report.pdf" from
// different folders) produce duplicate entries that most unzip tools silently
// overwrite. seen tracks assigned names across the archive.
func uniqueZipName(seen map[string]int, original string) string {
	base := filepath.Base(original)
	if seen[base] == 0 {
		seen[base] = 1
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := seen[base]; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if seen[candidate] == 0 {
			seen[candidate] = 1
			seen[base] = i + 1
			return candidate
		}
	}
}
