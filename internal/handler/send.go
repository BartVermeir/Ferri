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

// ── Send form ─────────────────────────────────────────────────────────────────

// homePageData is the template data for the combined send/request page.
type homePageData struct {
	baseData
	ExpiryOptions  []config.ExpiryOption
	WelcomeMessage string
	Mode           string // "send" or "request"
	Error          string // request panel validation error
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
		// The JS client sends FormData which the browser encodes as multipart/form-data.
		// ParseMultipartForm handles this. It also calls ParseForm internally so
		// URL-encoded fallback submissions work too.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			if err2 := r.ParseForm(); err2 != nil {
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
		if !isValidEmail(senderEmail) {
			jsonError(w, "Valid sender email is required", http.StatusBadRequest)
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
			for _, r := range recipients {
				if !isValidEmail(r) {
					jsonError(w, fmt.Sprintf("Invalid recipient email: %s", r), http.StatusBadRequest)
					return
				}
			}
		}

		// Parse file metadata from form — browser JS sends one entry per file:
		// filenames[]=foo.mov&filenames[]=bar.mxf&sizes[]=12345&sizes[]=67890
		filenames := r.Form["filenames[]"]
		sizesStr := r.Form["sizes[]"]

		if len(filenames) == 0 {
			jsonError(w, "At least one file is required", http.StatusBadRequest)
			return
		}
		if len(filenames) != len(sizesStr) {
			jsonError(w, "Filenames and sizes count mismatch", http.StatusBadRequest)
			return
		}
		if len(filenames) > cfg.Limits.MaxFilesPerTransfer {
			jsonError(w, fmt.Sprintf("Maximum %d files per transfer", cfg.Limits.MaxFilesPerTransfer),
				http.StatusBadRequest)
			return
		}

		// Parse and validate file sizes
		var files []store.CreateFileInput
		for i, name := range filenames {
			name = strings.TrimSpace(name)
			if name == "" {
				name = "unnamed"
			}

			size, err := strconv.ParseInt(sizesStr[i], 10, 64)
			if err != nil || size < 0 {
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
			NotifyRecipients: !linkOnly,
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

// isValidEmail is a minimal email validator. It checks for the presence of
// exactly one '@' with non-empty local and domain parts. Full RFC 5322
// validation is deliberately avoided — it is vastly complex and rejects
// addresses that real mail servers accept. The SMTP relay will reject
// truly invalid addresses at send time.
func isValidEmail(email string) bool {
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

func renderSendPage(w http.ResponseWriter, cfg *config.Config, settings *store.Settings, errMsg string) {
	renderHomePage(w, cfg, settings, "send", errMsg)
}
