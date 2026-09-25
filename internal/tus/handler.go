package tus

// TUS handler for Ferri.
//
// Architecture summary (see architecture.md §4 for full spec):
//
//   PreUploadCreateCallback — fires before any bytes are written.
//     Reads transfer_id or upload_request_token from TUS metadata.
//     Validates: record exists, correct status, not expired.
//     Enforces Upload-Length against limits.max_upload_bytes, the free space
//     on storage (limits.min_free_bytes) and, for transfers, the number of
//     files /send announced.
//     Creates a Ferri file row and injects the file ID into TUS metadata
//     so subsequent hooks can find it without a DB lookup by TUS upload ID.
//
//   handleCreated — fires after tusd creates the upload resource.
//     Records the tusd upload ID on the Ferri file row.
//
//   handleCompletions — fires after the final chunk arrives.
//     Marks file complete. Atomically activates transfer (race-safe).
//     If this goroutine won: enqueues mail_queue rows.
//
//   ServeHTTP post-PATCH hook — updates tus_last_activity_at after each chunk.
//
// Storage layout: flat. The data of an upload is <root>/<tus_upload_id>
// with <root>/<tus_upload_id>.info next to it. The transfers/<id>/ and
// requests/<id>/ folders are created but stay empty; a file row's
// storage_path (transfers/<transfer_id>/<file_id>) is a logical name, not
// where the bytes are.

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
	"github.com/BartVermeir/Ferri/internal/token"
)

const (
	// metaKeyFileID is injected into TUS upload metadata by PreUploadCreateCallback.
	// It holds the Ferri file/request-file row ID, allowing UploadFinisher and
	// post-PATCH hooks to locate the correct DB row without a secondary DB lookup.
	metaKeyFileID = "ferri_file_id"

	metaContextTransfer = "transfer"
	metaContextRequest  = "request"
)

// Handler wraps tusd and wires Ferri's token validation and DB callbacks.
type Handler struct {
	cfg     *config.Config
	stores  *store.Stores
	mgr     *storage.Manager
	handler *tusd.Handler
}

// NewHandler creates and configures the TUS HTTP handler.
// Storage directories are created if they do not exist.
func NewHandler(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) (*Handler, error) {
	for _, sub := range []string{"transfers", "requests"} {
		if err := mgr.MkdirAll(sub); err != nil {
			return nil, fmt.Errorf("tus: create storage dir %s: %w", sub, err)
		}
	}

	h := &Handler{cfg: cfg, stores: stores, mgr: mgr}

	// tusd DataStore: backed by the storage Manager (local or SMB).
	// The managerTUSStore delegates to whatever backend is active at call time.
	tusDataStore := mgr.TUSDataStore()
	// memorylocker: in-memory locking, prevents concurrent writes to the same upload.
	// For production with multiple instances, replace with filelocker or redislocker.
	locker := memorylocker.New()

	composer := tusd.NewStoreComposer()
	composer.UseCore(tusDataStore)
	locker.UseIn(composer)

	tusConfig := tusd.Config{
		BasePath:                "/tus/",
		StoreComposer:           composer,
		MaxSize:                 cfg.Limits.MaxUploadBytes,
		DisableDownload:         true,
		DisableTermination:      true, // disable DELETE — not needed, cleanup job handles it
		NotifyCompleteUploads:   true,
		NotifyCreatedUploads:    true,
		RespectForwardedHeaders: true, // trust nginx's X-Forwarded-Proto so upload URLs are https://, not http://
		PreUploadCreateCallback: func(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
			return h.preUploadCreate(hook)
		},
	}

	tusHandler, err := tusd.NewHandler(tusConfig)
	if err != nil {
		return nil, fmt.Errorf("tus: create handler: %w", err)
	}

	h.handler = tusHandler

	go h.handleCreated(tusHandler.CreatedUploads)
	go h.handleCompletions(tusHandler.CompleteUploads)

	return h, nil
}

// ServeHTTP satisfies http.Handler.
// After a successful PATCH, records activity on the file row.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	h.handler.ServeHTTP(rw, r)

	if r.Method == http.MethodPatch && rw.status >= 200 && rw.status < 300 {
		h.recordPatchActivity(r)
	}
}

// ── PreUploadCreateCallback ───────────────────────────────────────────────────

func (h *Handler) preUploadCreate(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	meta := hook.Upload.MetaData
	size := hook.Upload.Size

	if transferID, ok := meta["transfer_id"]; ok {
		return h.preCreateTransferFile(transferID, meta, size)
	}
	if reqToken, ok := meta["upload_request_token"]; ok {
		return h.preCreateRequestFile(reqToken, meta, size)
	}

	return rejectWith(http.StatusBadRequest,
		"missing transfer_id or upload_request_token in TUS metadata")
}

func (h *Handler) preCreateTransferFile(
	transferID string,
	meta tusd.MetaData,
	uploadLength int64,
) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	valid, err := h.stores.Transfers.ValidateForTUS(transferID)
	if err != nil {
		slog.Error("tus: validate transfer", "transfer_id", transferID, "error", err)
		return rejectWith(http.StatusInternalServerError, "internal error")
	}
	if !valid {
		return rejectWith(http.StatusForbidden, "transfer not found, expired, or already active")
	}

	if uploadLength > h.cfg.Limits.MaxUploadBytes {
		return rejectWith(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file size %d exceeds limit of %d bytes", uploadLength, h.cfg.Limits.MaxUploadBytes))
	}

	count, expected, err := h.stores.Transfers.CountFiles(transferID)
	if err != nil {
		slog.Error("tus: count transfer files", "transfer_id", transferID, "error", err)
		return rejectWith(http.StatusInternalServerError, "internal error")
	}
	if max := maxTransferFileRows(expected, h.cfg.Limits.MaxFilesPerTransfer); count >= max {
		slog.Warn("tus: transfer file limit reached", "transfer_id", transferID, "files", count, "max", max)
		return rejectWith(http.StatusForbidden, "This transfer already has all the files it announced.")
	}

	if h.freeSpaceShort(uploadLength) {
		return rejectWith(http.StatusInsufficientStorage, msgStorageFull)
	}

	originalName := stringMeta(meta, "filename")
	if originalName == "" {
		originalName = "unnamed"
	}

	fileID := token.Generate()
	storagePath := "transfers/" + transferID + "/" + fileID

	if err := h.mgr.MkdirAll("transfers/" + transferID); err != nil {
		slog.Error("tus: mkdir", "error", err)
		return rejectWith(http.StatusInternalServerError, "storage error")
	}

	if err := h.stores.Transfers.CreateFileRow(fileID, transferID, originalName, storagePath, uploadLength); err != nil {
		slog.Error("tus: create file row", "file_id", fileID, "error", err)
		return rejectWith(http.StatusInternalServerError, "database error")
	}

	slog.Info("tus: upload created", "file_id", fileID, "transfer_id", transferID,
		"name", originalName, "size", uploadLength)

	return tusd.HTTPResponse{}, tusd.FileInfoChanges{
		MetaData: tusd.MetaData{
			metaKeyFileID: fileID,
			"context":     metaContextTransfer,
			"transfer_id": transferID,
		},
	}, nil
}

func (h *Handler) preCreateRequestFile(
	uploadToken string,
	meta tusd.MetaData,
	uploadLength int64,
) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	requestID, valid, err := h.stores.Requests.ValidateForTUS(uploadToken)
	if err != nil {
		slog.Error("tus: validate request token", "error", err)
		return rejectWith(http.StatusInternalServerError, "internal error")
	}
	if !valid {
		return rejectWith(http.StatusForbidden, "upload request not found or expired")
	}

	if uploadLength > h.cfg.Limits.MaxUploadBytes {
		return rejectWith(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file size %d exceeds limit of %d bytes", uploadLength, h.cfg.Limits.MaxUploadBytes))
	}

	if h.freeSpaceShort(uploadLength) {
		return rejectWith(http.StatusInsufficientStorage, msgStorageFull)
	}

	originalName := stringMeta(meta, "filename")
	if originalName == "" {
		originalName = "unnamed"
	}

	fileID := token.Generate()
	storagePath := "requests/" + requestID + "/" + fileID

	if err := h.mgr.MkdirAll("requests/" + requestID); err != nil {
		slog.Error("tus: mkdir", "error", err)
		return rejectWith(http.StatusInternalServerError, "storage error")
	}

	if err := h.stores.Requests.CreateFileRow(fileID, requestID, originalName, storagePath, uploadLength); err != nil {
		slog.Error("tus: create request file row", "file_id", fileID, "error", err)
		return rejectWith(http.StatusInternalServerError, "database error")
	}

	slog.Info("tus: request upload created", "file_id", fileID, "request_id", requestID,
		"name", originalName, "size", uploadLength)

	return tusd.HTTPResponse{}, tusd.FileInfoChanges{
		MetaData: tusd.MetaData{
			metaKeyFileID: fileID,
			"context":     metaContextRequest,
			"request_id":  requestID,
		},
	}, nil
}

// maxTransferFileRows is how many file rows a transfer may get: twice what
// /send announced. Not the exact count, because tus-js-client re-POSTs when a
// create response is lost, leaving a dead 'uploading' row behind; an exact cap
// would then refuse the transfer's last real file and it would never go live.
// Pre-004 transfers (no expected_files) fall back to max_files_per_transfer.
func maxTransferFileRows(expected sql.NullInt64, maxFilesPerTransfer int) int {
	n := maxFilesPerTransfer
	if expected.Valid {
		n = int(expected.Int64)
	}
	return 2 * n
}

// freeSpaceShort reports whether an upload would leave less than
// limits.min_free_bytes free on storage: a full share breaks every upload at
// once. When the backend cannot report its free space the upload goes ahead
// (logged): failing closed would stop all uploads on such a server.
func (h *Handler) freeSpaceShort(uploadLength int64) bool {
	free, err := h.mgr.FreeSpace()
	if err != nil {
		slog.Warn("tus: cannot read free space on storage, allowing upload", "error", err)
		return false
	}
	if free < uint64(uploadLength)+uint64(h.cfg.Limits.MinFreeBytes) {
		slog.Warn("tus: not enough free space on storage", "free_bytes", free,
			"upload_bytes", uploadLength, "min_free_bytes", h.cfg.Limits.MinFreeBytes)
		return true
	}
	return false
}

// Shown to internal senders and external uploaders alike, so it names no one.
const msgStorageFull = "The server does not have enough free space for this file right now. Please try again later."

// ── Created hook ──────────────────────────────────────────────────────────────

func (h *Handler) handleCreated(ch <-chan tusd.HookEvent) {
	for event := range ch {
		fileID := event.Upload.MetaData[metaKeyFileID]
		ctx := event.Upload.MetaData["context"]
		tusID := event.Upload.ID

		if fileID == "" {
			slog.Warn("tus: handleCreated missing file_id", "tus_id", tusID)
			continue
		}

		var err error
		if ctx == metaContextTransfer {
			err = h.stores.Transfers.SetTUSUploadID(fileID, tusID)
		} else {
			err = h.stores.Requests.SetTUSUploadID(fileID, tusID)
		}
		if err != nil {
			slog.Error("tus: set tus_upload_id", "file_id", fileID, "tus_id", tusID, "error", err)
		} else {
			slog.Info("tus: tus_upload_id stored", "file_id", fileID, "tus_id", tusID)
		}
	}
}

// ── Completion hook ───────────────────────────────────────────────────────────

func (h *Handler) handleCompletions(ch <-chan tusd.HookEvent) {
	for event := range ch {
		if err := h.onUploadComplete(event); err != nil {
			slog.Error("tus: completion handler", "tus_id", event.Upload.ID, "error", err)
		}
	}
}

func (h *Handler) onUploadComplete(event tusd.HookEvent) error {
	meta := event.Upload.MetaData
	fileID := meta[metaKeyFileID]
	ctx := meta["context"]
	tusID := event.Upload.ID

	if fileID == "" {
		return errors.New("completion event missing ferri_file_id in metadata")
	}

	size := event.Upload.Size

	// Store the tusd upload ID here as well — the handleCreated hook may have
	// a timing issue. The completion event is guaranteed to fire after the upload
	// is fully written, so this is the reliable place to store the tusd upload ID.
	if ctx == metaContextTransfer {
		if err := h.stores.Transfers.SetTUSUploadID(fileID, tusID); err != nil {
			slog.Error("tus: set tus_upload_id on complete", "file_id", fileID, "tus_id", tusID, "error", err)
		} else {
			slog.Info("tus: tus_upload_id set on complete", "file_id", fileID, "tus_id", tusID)
		}
		return h.completeTransferFile(fileID, meta["transfer_id"], size)
	}
	if err := h.stores.Requests.SetTUSUploadID(fileID, tusID); err != nil {
		slog.Error("tus: set request tus_upload_id on complete", "file_id", fileID, "tus_id", tusID, "error", err)
	}
	return h.completeRequestFile(fileID, meta["request_id"], size)
}

func (h *Handler) completeTransferFile(fileID, transferID string, size int64) error {
	if err := h.stores.Transfers.SetFileComplete(fileID, size); err != nil {
		return fmt.Errorf("set file complete: %w", err)
	}

	slog.Info("tus: file complete", "file_id", fileID, "transfer_id", transferID, "size", size)

	// Atomic activation — only one goroutine wins when multiple files finish concurrently.
	activated, err := h.stores.Transfers.TryActivate(transferID)
	if err != nil {
		return fmt.Errorf("try activate: %w", err)
	}
	if !activated {
		return nil // another file still uploading, or race already won by sibling
	}

	slog.Info("tus: transfer activated", "transfer_id", transferID)

	if err := h.enqueueTransferMails(transferID); err != nil {
		// Log only — transfer is activated, mail failure is recoverable.
		slog.Error("tus: enqueue mails", "transfer_id", transferID, "error", err)
	}

	return nil
}

func (h *Handler) completeRequestFile(fileID, requestID string, size int64) error {
	if err := h.stores.Requests.SetFileComplete(fileID, size); err != nil {
		return fmt.Errorf("set request file complete: %w", err)
	}

	slog.Info("tus: request file complete", "file_id", fileID, "request_id", requestID, "size", size)

	return nil
}

// ── Mail enqueue ──────────────────────────────────────────────────────────────

func (h *Handler) enqueueTransferMails(transferID string) error {
	settings := h.stores.Settings.Get()
	if settings.MailFromAddress == "" {
		return nil
	}

	t, err := h.stores.Transfers.GetByID(transferID)
	if err != nil || t == nil {
		return fmt.Errorf("get transfer for mail: %w", err)
	}

	// Link-only transfers: no notification emails, no sender confirmation.
	if !t.NotifyRecipients {
		return nil
	}

	all, err := h.stores.Transfers.GetRecipients(transferID)
	if err != nil {
		return fmt.Errorf("get recipients: %w", err)
	}
	dbFiles, err := h.stores.Transfers.GetFilesByTransferID(transferID)
	if err != nil {
		return fmt.Errorf("get files: %w", err)
	}

	m := transferMail{
		SenderName:  t.SenderName,
		SenderEmail: t.SenderEmail,
		Title:       t.Title,
		Message:     t.Message,
		ExpiresAt:   t.ExpiresAt,
		Password:    t.PasswordHash.Valid,
		Loc:         h.cfg.Server.Location,
		BaseURL:     h.cfg.Server.BaseURL,
		Settings:    settings,
	}
	for _, f := range dbFiles {
		// A stray 'uploading' row (a TUS client that restarted an upload after
		// a 404) is not part of what the recipients receive.
		if f.Status != "complete" {
			continue
		}
		m.Files = append(m.Files, mail.FileItem{Name: f.OriginalName, Size: f.SizeBytes})
	}

	// The sender's link is the is_sender row, or — when the sender listed
	// themselves as a recipient — that recipient row (see SenderLink).
	var recipients []store.Recipient
	senderURL := ""
	for _, r := range all {
		if r.IsSender {
			senderURL = m.BaseURL + "/dl/" + r.DownloadToken
			continue
		}
		recipients = append(recipients, r)
		if senderURL == "" && strings.EqualFold(r.Email, t.SenderEmail) {
			senderURL = m.BaseURL + "/dl/" + r.DownloadToken
		}
	}

	for _, r := range recipients {
		downloadURL := m.BaseURL + "/dl/" + r.DownloadToken
		subject := fmt.Sprintf("%s shared %s with you", m.sender(), m.subjectTitle())
		bodyHTML := buildAvailableHTML(m, downloadURL)
		bodyText := buildAvailableText(m, downloadURL)

		if err := h.stores.Mail.Enqueue(nil, r.Email, subject, bodyHTML, bodyText); err != nil {
			slog.Error("tus: enqueue recipient mail", "to", r.Email, "error", err)
		}

		if err := h.stores.Transfers.MarkRecipientNotified(r.ID); err != nil {
			slog.Error("tus: mark recipient notified", "recipient_id", r.ID, "error", err)
		}
	}

	var emails []string
	for _, r := range recipients {
		emails = append(emails, r.Email)
	}

	// Sender confirmation
	subject := fmt.Sprintf("Sent: %s to %s", m.subjectTitle(), mail.Plural(len(emails), "recipient"))
	bodyHTML := buildConfirmHTML(m, emails, senderURL)
	bodyText := buildConfirmText(m, emails, senderURL)
	if err := h.stores.Mail.Enqueue(nil, t.SenderEmail, subject, bodyHTML, bodyText); err != nil {
		slog.Error("tus: enqueue sender confirmation", "to", t.SenderEmail, "error", err)
	}

	return nil
}

// ── Post-PATCH activity ───────────────────────────────────────────────────────

func (h *Handler) recordPatchActivity(r *http.Request) {
	tusID := path.Base(r.URL.Path)
	if tusID == "" || tusID == "." || tusID == "/" {
		return
	}

	// Try transfer files first
	fileID, err := h.stores.Transfers.GetFileIDByTUSID(tusID)
	if err == nil && fileID != "" {
		if err := h.stores.Transfers.UpdateTUSActivity(fileID); err != nil {
			slog.Error("tus: update transfer activity", "file_id", fileID, "error", err)
		}
		return
	}

	// Try request files
	fileID, err = h.stores.Requests.GetFileIDByTUSID(tusID)
	if err == nil && fileID != "" {
		if err := h.stores.Requests.UpdateTUSActivity(fileID); err != nil {
			slog.Error("tus: update request activity", "file_id", fileID, "error", err)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// rejectWith refuses an upload from the pre-create hook. The error must be a
// tusd.Error: for any other error tusd drops the returned response and answers
// 500, which tus-js-client then retries and shows as "unexpected response".
// The body is msg alone, so upload.js can show it to the user as is.
func rejectWith(status int, msg string) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	resp := tusd.HTTPResponse{
		StatusCode: status,
		Body:       msg + "\n",
		Header:     tusd.HTTPHeader{"Content-Type": "text/plain; charset=utf-8"},
	}
	return resp, tusd.FileInfoChanges{}, tusd.Error{ErrorCode: "ERR_UPLOAD_REJECTED", Message: msg, HTTPResponse: resp}
}

func stringMeta(meta tusd.MetaData, key string) string {
	v, _ := meta[key]
	return v
}

// responseRecorder captures the status code set by tusd for the post-PATCH hook.
type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// ── Mail body builders ────────────────────────────────────────────────────────

// transferMail holds everything the transfer mails show.
type transferMail struct {
	SenderName  string
	SenderEmail string
	Title       string
	Message     string
	Files       []mail.FileItem
	ExpiresAt   time.Time
	Password    bool
	Loc         *time.Location
	BaseURL     string
	Settings    *store.Settings
}

func (m transferMail) sender() string {
	if m.SenderName != "" {
		return m.SenderName
	}
	if m.SenderEmail != "" {
		return m.SenderEmail
	}
	return "Someone"
}

// subjectTitle is the quoted title, or "N files" when the sender left the
// (optional) title empty.
func (m transferMail) subjectTitle() string {
	if m.Title != "" {
		return `"` + m.Title + `"`
	}
	return mail.Plural(len(m.Files), "file")
}

func buildAvailableHTML(m transferMail, downloadURL string) string {
	company := mail.CompanyName(m.Settings)

	from := "<strong>" + html.EscapeString(m.sender()) + "</strong>"
	if m.SenderName != "" && m.SenderEmail != "" {
		from += ` (<a href="mailto:` + html.EscapeString(m.SenderEmail) + `" style="color:#555;">` + html.EscapeString(m.SenderEmail) + `</a>)`
	}

	var b strings.Builder
	b.WriteString(`<p style="margin:0 0 16px;">Hello,</p>`)
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s has shared %s with you through %s.</p>`,
		from, mail.Plural(len(m.Files), "file"), html.EscapeString(company))
	if m.Title != "" {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;font-size:18px;font-weight:600;color:#1a1a1a;line-height:1.3;">%s</p>`, html.EscapeString(m.Title))
	}
	b.WriteString(mail.QuoteHTML(m.Message))
	b.WriteString(mail.FileListHTML(m.Files))
	b.WriteString(mail.ButtonHTML(downloadURL, "Download files", m.Settings))
	fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">The files are available until <strong>%s</strong>. After that date the link stops working.</p>`,
		html.EscapeString(mail.FormatDate(m.ExpiresAt, m.Loc)))
	if m.Password {
		fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">This transfer is password protected. You need the password from %s to open it.</p>`,
			html.EscapeString(m.sender()))
	}
	b.WriteString(mail.NoteHTML(fmt.Sprintf(
		"You are receiving this email because %s entered your address to send you files. This link is personal to you, please do not forward it. If you were not expecting these files, you can ignore this email.",
		m.sender())))

	preheader := fmt.Sprintf("%s shared %s with you, available until %s.",
		m.sender(), mail.Plural(len(m.Files), "file"), mail.FormatDate(m.ExpiresAt, m.Loc))
	return mail.Wrap(m.Settings, m.BaseURL, preheader, b.String())
}

func buildAvailableText(m transferMail, downloadURL string) string {
	var b strings.Builder
	b.WriteString("Hello,\n\n")
	from := m.sender()
	if m.SenderName != "" && m.SenderEmail != "" {
		from += " (" + m.SenderEmail + ")"
	}
	fmt.Fprintf(&b, "%s has shared %s with you through %s.\n\n",
		from, mail.Plural(len(m.Files), "file"), mail.CompanyName(m.Settings))
	if m.Title != "" {
		b.WriteString(m.Title + "\n\n")
	}
	if m.Message != "" {
		b.WriteString(m.Message + "\n\n")
	}
	if len(m.Files) > 0 {
		b.WriteString("Files:\n" + mail.FileListText(m.Files) + "\n")
	}
	fmt.Fprintf(&b, "Download: %s\n\n", downloadURL)
	fmt.Fprintf(&b, "The files are available until %s. After that date the link stops working.\n",
		mail.FormatDate(m.ExpiresAt, m.Loc))
	if m.Password {
		fmt.Fprintf(&b, "This transfer is password protected. You need the password from %s to open it.\n", m.sender())
	}
	fmt.Fprintf(&b, "\n--\nYou are receiving this email because %s entered your address to send you files. This link is personal to you, please do not forward it. If you were not expecting these files, you can ignore this email.\n",
		m.sender())
	return b.String()
}

func buildConfirmHTML(m transferMail, recipients []string, senderURL string) string {
	var b strings.Builder
	if m.SenderName != "" {
		fmt.Fprintf(&b, `<p style="margin:0 0 16px;">Hello %s,</p>`, html.EscapeString(m.SenderName))
	} else {
		b.WriteString(`<p style="margin:0 0 16px;">Hello,</p>`)
	}
	titlePart := ""
	if m.Title != "" {
		titlePart = " <strong>" + html.EscapeString(m.Title) + "</strong>"
	}
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">Your transfer%s has been uploaded and a download link was emailed to %s.</p>`,
		titlePart, mail.Plural(len(recipients), "recipient"))

	b.WriteString(`<p style="margin:0 0 6px;font-size:12px;font-weight:600;color:#888;text-transform:uppercase;letter-spacing:0.04em;">Sent to</p>`)
	b.WriteString(`<p style="margin:0 0 20px;font-size:13px;color:#333;line-height:1.7;">`)
	for i, r := range recipients {
		if i > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(html.EscapeString(r))
	}
	b.WriteString(`</p>`)

	b.WriteString(`<p style="margin:0 0 6px;font-size:12px;font-weight:600;color:#888;text-transform:uppercase;letter-spacing:0.04em;">Files</p>`)
	b.WriteString(mail.FileListHTML(m.Files))

	if senderURL != "" {
		b.WriteString(`<p style="margin:0 0 12px;font-size:13px;color:#555;">Your own link to the transfer, to check or download the files yourself:</p>`)
		b.WriteString(mail.ButtonHTML(senderURL, "View transfer", m.Settings))
	}
	fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">Available until <strong>%s</strong>.</p>`,
		html.EscapeString(mail.FormatDate(m.ExpiresAt, m.Loc)))
	b.WriteString(mail.NoteHTML(confirmNote(m.Settings, senderURL != "")))

	preheader := fmt.Sprintf("Sent to %s, available until %s.",
		mail.Plural(len(recipients), "recipient"), mail.FormatDate(m.ExpiresAt, m.Loc))
	return mail.Wrap(m.Settings, m.BaseURL, preheader, b.String())
}

func buildConfirmText(m transferMail, recipients []string, senderURL string) string {
	var b strings.Builder
	if m.SenderName != "" {
		fmt.Fprintf(&b, "Hello %s,\n\n", m.SenderName)
	} else {
		b.WriteString("Hello,\n\n")
	}
	titlePart := ""
	if m.Title != "" {
		titlePart = " \"" + m.Title + "\""
	}
	fmt.Fprintf(&b, "Your transfer%s has been uploaded and a download link was emailed to %s.\n\n",
		titlePart, mail.Plural(len(recipients), "recipient"))
	b.WriteString("Sent to:\n")
	for _, r := range recipients {
		b.WriteString("  - " + r + "\n")
	}
	if len(m.Files) > 0 {
		b.WriteString("\nFiles:\n" + mail.FileListText(m.Files))
	}
	if senderURL != "" {
		fmt.Fprintf(&b, "\nYour own link to the transfer: %s\n", senderURL)
	}
	fmt.Fprintf(&b, "\nAvailable until %s.\n", mail.FormatDate(m.ExpiresAt, m.Loc))
	fmt.Fprintf(&b, "\n--\n%s\n", confirmNote(m.Settings, senderURL != ""))
	return b.String()
}

// confirmNote tells the sender which follow-up mails to expect, so they
// match what is actually switched on in the admin settings.
func confirmNote(settings *store.Settings, hasSenderLink bool) string {
	var parts []string
	if hasSenderLink {
		parts = append(parts, "Downloads through your own link are not counted as recipient downloads.")
	}
	if settings != nil && settings.NotifyOnDownload {
		parts = append(parts, "You will get an email when a recipient downloads from this transfer (at most one per recipient per hour, however many files).")
	}
	if settings != nil && settings.ExpirySummary {
		parts = append(parts, "When the transfer expires you will receive a summary of who downloaded what.")
	}
	if len(parts) == 0 {
		return "You are receiving this email because you sent files with " + mail.CompanyName(settings) + "."
	}
	return strings.Join(parts, " ")
}
