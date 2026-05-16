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

	"github.com/your-org/ferri/internal/config"
	appMiddleware "github.com/your-org/ferri/internal/middleware"
	"github.com/your-org/ferri/internal/store"
)

// ── Send form ─────────────────────────────────────────────────────────────────

// SendPage handles GET /.
// Renders the send form with expiry options from config and branding from settings.
func SendPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		renderSendPage(w, cfg, settings, "")
	}
}

// ── Transfer creation ─────────────────────────────────────────────────────────

// SendCreate handles POST /send.
// Validates input, creates the transfer in the DB, returns JSON {transfer_id}.
func SendCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil { // 1 MB limit for form data
			jsonError(w, "Invalid form data", http.StatusBadRequest)
			return
		}

		// ── Input validation ──────────────────────────────────────────────────

		senderName := strings.TrimSpace(r.FormValue("sender_name"))
		senderEmail := strings.TrimSpace(r.FormValue("sender_email"))
		title := strings.TrimSpace(r.FormValue("title"))
		message := strings.TrimSpace(r.FormValue("message"))
		password := r.FormValue("password")
		expiryStr := r.FormValue("expiry_hours")
		recipientsRaw := r.FormValue("recipients") // comma or newline separated

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

		// Parse recipients — comma or newline separated, trim whitespace
		recipients := parseRecipients(recipientsRaw)
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

			// storage_path is set here as a placeholder; the TUS handler writes
			// to transfers/<transfer_id>/<file_id> — the exact path is determined
			// by the file ID which is generated in store.TransferStore.Create.
			// We pass an empty storage path now; the TUS PreUploadCreateCallback
			// sets the real path when it creates the file row.
			// Wait — actually TransferStore.Create creates the file rows with the
			// storage path. But we don't know the transfer_id or file_id yet.
			// Solution: pass a placeholder storage path here; TUS CreateFileRow
			// sets the real path and the file row created here is replaced.
			// Actually: we should NOT pre-create file rows here. The TUS handler
			// creates file rows in PreUploadCreateCallback with the correct path.
			// POST /send only creates the transfer row and recipient rows.
			// File rows are created by the TUS handler when uploads start.
			// So: we validate file metadata here but don't create file rows.
			files = append(files, store.CreateFileInput{
				OriginalName: name,
				StoragePath:  "", // set by TUS handler
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
			Title:        title,
			Message:      message,
			SenderName:   senderName,
			SenderEmail:  senderEmail,
			PasswordHash: passwordHash,
			ExpiresAt:    expiresAt,
			Recipients:   recipients,
			// Files are NOT created here — TUS PreUploadCreateCallback creates
			// file rows with the correct storage path when each upload starts.
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
		)

		// Return transfer_id to browser JS — it drives the TUS uploads from here.
		jsonOK(w, map[string]any{
			"transfer_id":    result.TransferID,
			"file_count":     len(files),
			"recipient_count": len(result.Recipients),
		})
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Build expiry options HTML
	var opts strings.Builder
	for _, opt := range cfg.ExpiryOptions {
		opts.WriteString(fmt.Sprintf(
			`<option value="%d">%s</option>`, opt.Hours, opt.Label,
		))
	}

	pageTitle := settings.SendPageTitle
	if pageTitle == "" {
		pageTitle = "Send files"
	}

	welcomeMsg := ""
	if settings.WelcomeMessage != "" {
		welcomeMsg = fmt.Sprintf("<p>%s</p>", settings.WelcomeMessage)
	}

	errHTML := ""
	if errMsg != "" {
		errHTML = fmt.Sprintf(`<p style="color:red">%s</p>`, errMsg)
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <title>%s — %s</title>
  <meta charset="utf-8">
</head>
<body>
  <h1>%s</h1>
  %s
  %s
  <form id="send-form">
    <label>Your name<br><input type="text" name="sender_name" required></label><br><br>
    <label>Your email<br><input type="email" name="sender_email" required></label><br><br>
    <label>Title<br><input type="text" name="title"></label><br><br>
    <label>Message<br><textarea name="message"></textarea></label><br><br>
    <label>Recipients (one per line or comma-separated)<br>
      <textarea name="recipients" required></textarea>
    </label><br><br>
    <label>Files<br><input type="file" name="files" multiple required></label><br><br>
    <label>Expires after
      <select name="expiry_hours">%s</select>
    </label><br><br>
    <label>Password (optional)<br><input type="password" name="password"></label><br><br>
    <button type="submit">Send files</button>
  </form>
  <script src="/static/upload.js"></script>
</body>
</html>`,
		pageTitle, settings.CompanyName,
		pageTitle,
		welcomeMsg,
		errHTML,
		opts.String(),
	)
}
