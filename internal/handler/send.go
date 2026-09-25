package handler

// Send handler for Ferri.
//
// Routes (IP-restricted — internal network only):
//   GET  /        — render send form
//   POST /send    — create transfer, return JSON {transfer_id}
//
// Flow (architecture.md §5a):
//   1. GET / renders the send form with expiry options and branding.
//   2. POST /send validates input, hashes password if provided,
//      creates transfer + file rows + recipient rows in one transaction,
//      returns JSON {transfer_id} to browser JS.
//   3. Browser JS uses transfer_id to drive TUS uploads via POST /tus/.
//      No per-file tokens are returned here — tusd generates upload IDs itself.
//   4. TUS UploadFinisher handles activation and mail enqueue.
//
// POST /send returns JSON, not a redirect, because the browser JS
// needs the transfer_id to start TUS uploads immediately.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/store"
)

// Input bounds shared by the send and request forms. These are sanity limits to
// keep one POST from creating an unbounded number of recipient / mail_queue rows
// or storing megabytes of free text — not business rules.
const (
	maxRecipients = 100
	maxTitleLen   = 200
	maxMessageLen = 5000
	maxNameLen    = 200
	// maxFormBytes caps a /request body, maxSendBytes a /send body. /send
	// carries the file list: up to max_files_per_transfer (5000) paths in
	// folders, a few hundred bytes each at most. Without a cap, a multipart
	// body went to /tmp, which is RAM in the container (audit L11).
	maxFormBytes = 1 << 20
	maxSendBytes = 4 << 20
)

// ── Send form ─────────────────────────────────────────────────────────────────

// homePageData is the template data for the combined send/request page.
type homePageData struct {
	baseData
	ExpiryOptions  []config.ExpiryOption
	WelcomeMessage string
	Mode           string // "send" or "request"
	Error          string // request panel validation error
	// Limits for upload.js: at most MaxFiles files per transfer, folders
	// included (DEC-035), each at most MaxUploadBytes. The server enforces
	// both again.
	MaxFiles       int
	MaxUploadBytes int64
}

// renderHomePage renders the combined send/request page (send.html).
func renderHomePage(w http.ResponseWriter, cfg *config.Config, settings *store.Settings, mode, errMsg string) {
	if mode != "request" {
		mode = "send"
	}
	pageTitle := settings.SendPageTitle
	if pageTitle == "" {
		pageTitle = "Send files"
	}
	renderPage(w, "send.html", homePageData{
		baseData:       baseData{PageTitle: pageTitle, Settings: settings},
		ExpiryOptions:  cfg.ExpiryOptions,
		WelcomeMessage: settings.WelcomeMessage,
		Mode:           mode,
		Error:          errMsg,
		MaxFiles:       cfg.Limits.MaxFilesPerTransfer,
		MaxUploadBytes: cfg.Limits.MaxUploadBytes,
	})
}

// SendPage handles GET /.
// Renders the combined send/request page. ?mode=request pre-selects the request tab.
func SendPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		mode := r.URL.Query().Get("mode")
		renderHomePage(w, cfg, settings, mode, "")
	}
}

// ── Transfer creation ─────────────────────────────────────────────────────────

// SendCreate handles POST /send.
// Validates input, creates the transfer in the DB, returns JSON {transfer_id}.
func SendCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSendBytes)

		// The JS client sends FormData, which the browser encodes as
		// multipart/form-data. Only a non-multipart body may fall back to
		// ParseForm: a multipart body that fails to parse (e.g. Go's limit of
		// 1000 parts, hit by the old two-fields-per-file format at 500 files)
		// used to fall through with every field empty and report "Sender name
		// is required" (audit M12).
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			if !errors.Is(err, http.ErrNotMultipart) {
				slog.Warn("send: parse multipart form", "error", err)
				jsonError(w, "Could not read the form. Please reload the page and try again.", http.StatusBadRequest)
				return
			}
			if err := r.ParseForm(); err != nil {
				jsonError(w, "Invalid form data", http.StatusBadRequest)
				return
			}
		}

		// ── Input validation ──────────────────────────────────────────────────

		senderName := strings.TrimSpace(r.FormValue("sender_name"))
		senderEmail := strings.TrimSpace(r.FormValue("sender_email"))
		title := strings.TrimSpace(r.FormValue("title"))
		message := strings.TrimSpace(r.FormValue("message"))
		password := r.FormValue("password")
		expiryStr := r.FormValue("expiry_hours")
		recipientsRaw := r.FormValue("recipients") // comma or newline separated
		linkOnly := r.FormValue("link_only") == "1"

		if senderName == "" {
			jsonError(w, "Sender name is required", http.StatusBadRequest)
			return
		}
		if len(senderName) > maxNameLen {
			jsonError(w, fmt.Sprintf("Name is too long (max %d characters)", maxNameLen), http.StatusBadRequest)
			return
		}
		if !isValidEmail(senderEmail) {
			jsonError(w, "Valid sender email is required", http.StatusBadRequest)
			return
		}
		if len(title) > maxTitleLen {
			jsonError(w, fmt.Sprintf("Title is too long (max %d characters)", maxTitleLen), http.StatusBadRequest)
			return
		}
		if len(message) > maxMessageLen {
			jsonError(w, fmt.Sprintf("Message is too long (max %d characters)", maxMessageLen), http.StatusBadRequest)
			return
		}

		// Parse and validate expiry hours against configured options
		expiryHours, err := strconv.Atoi(expiryStr)
		if err != nil || !isValidExpiryOption(expiryHours, cfg.ExpiryOptions) {
			jsonError(w, "Invalid expiry option", http.StatusBadRequest)
			return
		}

		// Determine recipients
		var recipients []string
		if linkOnly {
			// Link-only: use sender as sole recipient to generate a download token.
			// No notification email will be sent.
			recipients = []string{senderEmail}
		} else {
			recipients = parseRecipients(recipientsRaw)
			if len(recipients) == 0 {
				jsonError(w, "At least one recipient is required", http.StatusBadRequest)
				return
			}
			if len(recipients) > maxRecipients {
				jsonError(w, fmt.Sprintf("Too many recipients (max %d)", maxRecipients), http.StatusBadRequest)
				return
			}
			for _, r := range recipients {
				if !isValidEmail(r) {
					jsonError(w, fmt.Sprintf("Invalid recipient email: %s", r), http.StatusBadRequest)
					return
				}
			}
		}

		announced, err := parseAnnouncedFiles(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(announced) == 0 {
			jsonError(w, "At least one file is required", http.StatusBadRequest)
			return
		}
		if len(announced) > cfg.Limits.MaxFilesPerTransfer {
			jsonError(w, fmt.Sprintf("Maximum %d files per transfer", cfg.Limits.MaxFilesPerTransfer),
				http.StatusBadRequest)
			return
		}

		// Parse and validate file sizes
		var files []store.CreateFileInput
		for _, a := range announced {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				name = "unnamed"
			}

			size := a.Size
			if size < 0 {
				jsonError(w, fmt.Sprintf("Invalid file size for %s", name), http.StatusBadRequest)
				return
			}
			if size > cfg.Limits.MaxUploadBytes {
				jsonError(w,
					fmt.Sprintf("File %s exceeds maximum size of %d bytes", name, cfg.Limits.MaxUploadBytes),
					http.StatusRequestEntityTooLarge)
				return
			}

			// Collect file metadata for validation and logging.
			// File rows are NOT created here — the TUS PreUploadCreateCallback
			// creates them with the correct storage path when each upload starts.
			files = append(files, store.CreateFileInput{
				OriginalName: name,
				SizeBytes:    size,
			})
		}

		// ── Password hashing ──────────────────────────────────────────────────

		var passwordHash string
		if password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				slog.Error("send: bcrypt hash", "error", err)
				jsonError(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			passwordHash = string(hash)
		}

		// ── Create transfer ───────────────────────────────────────────────────

		expiresAt := time.Now().Add(time.Duration(expiryHours) * time.Hour)

		input := store.CreateTransferInput{
			Title:            title,
			Message:          message,
			SenderName:       senderName,
			SenderEmail:      senderEmail,
			PasswordHash:     passwordHash,
			ExpiresAt:        expiresAt,
			Recipients:       recipients,
			ExpectedFiles:    len(files),
			NotifyRecipients: !linkOnly,
			// Link-only already makes the sender the sole recipient.
			SenderLink: !linkOnly,
		}

		result, err := stores.Transfers.Create(input)
		if err != nil {
			slog.Error("send: create transfer", "error", err)
			jsonError(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		slog.Info("send: transfer created",
			"transfer_id", result.TransferID,
			"sender", senderEmail,
			"recipients", len(recipients),
			"files", len(files),
			"expiry_hours", expiryHours,
			"link_only", linkOnly,
		)

		resp := map[string]any{
			"transfer_id":     result.TransferID,
			"file_count":      len(files),
			"recipient_count": len(result.Recipients),
			"link_only":       linkOnly,
		}
		// In link-only mode return the download URL so the JS can display it.
		if linkOnly && len(result.Recipients) > 0 {
			resp["download_url"] = cfg.Server.BaseURL + "/dl/" + result.Recipients[0].DownloadToken
		}
		jsonOK(w, resp)
	}
}

// ── Validation helpers ────────────────────────────────────────────────────────

// announcedFile is one file the browser is about to upload over TUS.
type announcedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// parseAnnouncedFiles reads the file list of POST /send: one JSON field
// `files` ([{"name":…,"size":…}]). One field, however many files — the old
// format used two form fields per file and broke at 500 files on Go's
// multipart part limit. That old format (filenames[] + sizes[]) is still
// accepted for a page that was open during a deploy.
func parseAnnouncedFiles(r *http.Request) ([]announcedFile, error) {
	if raw := r.FormValue("files"); raw != "" {
		var list []announcedFile
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			//lint:ignore ST1005 shown to the user as is
			return nil, errors.New("Invalid file list")
		}
		return list, nil
	}
	names, sizes := r.Form["filenames[]"], r.Form["sizes[]"]
	if len(names) != len(sizes) {
		//lint:ignore ST1005 shown to the user as is
		return nil, errors.New("Filenames and sizes count mismatch")
	}
	list := make([]announcedFile, 0, len(names))
	for i, name := range names {
		size, err := strconv.ParseInt(sizes[i], 10, 64)
		if err != nil {
			//lint:ignore ST1005 shown to the user as is
			return nil, fmt.Errorf("Invalid file size for %s", name)
		}
		list = append(list, announcedFile{Name: name, Size: size})
	}
	return list, nil
}

// isValidEmail is a minimal email validator. It checks for the presence of
// exactly one '@' with non-empty local and domain parts. Full RFC 5322
// validation is deliberately avoided — it is vastly complex and rejects
// addresses that real mail servers accept. The SMTP relay will reject
// truly invalid addresses at send time.
func isValidEmail(email string) bool {
	// No whitespace or control characters, and at most 254 characters: a CR
	// or LF passed here and only failed when the mail was sent, so the mail
	// failed for good (audit L11).
	if len(email) > 254 || strings.IndexFunc(email, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return false
	}
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	return len(parts[0]) > 0 && len(parts[1]) > 1 && strings.Contains(parts[1], ".")
}

// isValidExpiryOption checks that the submitted hours value matches one of the
// configured expiry options. This prevents a client from submitting an arbitrary
// expiry duration that bypasses the intended options.
func isValidExpiryOption(hours int, options []config.ExpiryOption) bool {
	for _, opt := range options {
		if opt.Hours == hours {
			return true
		}
	}
	return false
}

// parseRecipients splits a comma or newline separated string of email addresses,
// trims whitespace, removes empty strings, and deduplicates.
func parseRecipients(raw string) []string {
	// Normalise separators: replace newlines with commas
	raw = strings.ReplaceAll(raw, "\n", ",")
	raw = strings.ReplaceAll(raw, "\r", "")

	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{})
	var result []string
	for _, p := range parts {
		email := strings.ToLower(strings.TrimSpace(p))
		if email == "" {
			continue
		}
		if _, exists := seen[email]; exists {
			continue // deduplicate
		}
		seen[email] = struct{}{}
		result = append(result, email)
	}
	return result
}

// ── JSON response helpers ─────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("send: json encode", "error", err)
	}
}

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ── Template rendering placeholder ───────────────────────────────────────────
// Replace with real template rendering when web/templates/ are implemented.
