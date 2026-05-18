package tus

// TUS handler for Ferri.
//
// Architecture summary (see architecture.md §4 for full spec):
//
//   PreUploadCreateCallback — fires before any bytes are written.
//     Reads transfer_id or upload_request_token from TUS metadata.
//     Validates: record exists, correct status, not expired.
//     Enforces Upload-Length against limits.max_upload_bytes.
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
// Storage layout:
//   Transfer files: <storage_path>/transfers/<transfer_id>/<file_id>
//   Request files:  <storage_path>/requests/<request_id>/<file_id>

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
	"github.com/your-org/ferri/internal/token"
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
	handler *tusd.Handler
}

// NewHandler creates and configures the TUS HTTP handler.
// Storage directories are created if they do not exist.
func NewHandler(cfg *config.Config, stores *store.Stores) (*Handler, error) {
	for _, sub := range []string{"transfers", "requests"} {
		dir := filepath.Join(cfg.Storage.Path, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("tus: create storage dir %s: %w", dir, err)
		}
	}

	h := &Handler{cfg: cfg, stores: stores}

	// tusd filestore: writes upload data and .info sidecar files under StoragePath.
	// Ferri manages its own path layout (transfers/<id>/<file_id>) separately.
	fs := filestore.New(cfg.Storage.Path)
	// memorylocker: in-memory locking, prevents concurrent writes to the same upload.
	// For production with multiple instances, replace with filelocker or redislocker.
	locker := memorylocker.New()

	composer := tusd.NewStoreComposer()
	fs.UseIn(composer)
	locker.UseIn(composer)

	tusConfig := tusd.Config{
		BasePath:              "/tus/",
		StoreComposer:         composer,
		MaxSize:               cfg.Limits.MaxUploadBytes,
		DisableDownload:       true,
		DisableTermination:    false,
		NotifyCompleteUploads: true,
		NotifyCreatedUploads:  true,
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

	// Debug: log all received metadata keys
	metaKeys := make([]string, 0, len(meta))
	for k := range meta {
		metaKeys = append(metaKeys, k)
	}
	slog.Info("tus: preUploadCreate", "meta_keys", metaKeys, "size", size)

	if transferID, ok := meta["transfer_id"]; ok {
		return h.preCreateTransferFile(transferID, meta, size)
	}
	if reqToken, ok := meta["upload_request_token"]; ok {
		return h.preCreateRequestFile(reqToken, meta, size)
	}

	slog.Warn("tus: missing transfer_id or upload_request_token", "meta_keys", metaKeys)
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

	originalName := stringMeta(meta, "filename")
	if originalName == "" {
		originalName = "unnamed"
	}

	fileID := token.Generate()
	storagePath := filepath.Join("transfers", transferID, fileID)

	if err := os.MkdirAll(filepath.Join(h.cfg.Storage.Path, "transfers", transferID), 0o755); err != nil {
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

	originalName := stringMeta(meta, "filename")
	if originalName == "" {
		originalName = "unnamed"
	}

	fileID := token.Generate()
	storagePath := filepath.Join("requests", requestID, fileID)

	if err := os.MkdirAll(filepath.Join(h.cfg.Storage.Path, "requests", requestID), 0o755); err != nil {
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

// ── Created hook ──────────────────────────────────────────────────────────────

func (h *Handler) handleCreated(ch <-chan tusd.HookEvent) {
	for event := range ch {
		fileID := event.Upload.MetaData[metaKeyFileID]
		ctx := event.Upload.MetaData["context"]
		tusID := event.Upload.ID

		if fileID == "" {
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

	if fileID == "" {
		return errors.New("completion event missing ferri_file_id in metadata")
	}

	size := event.Upload.Size

	if ctx == metaContextTransfer {
		return h.completeTransferFile(fileID, meta["transfer_id"], size)
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

	recipients, err := h.stores.Transfers.GetRecipients(transferID)
	if err != nil {
		return fmt.Errorf("get recipients: %w", err)
	}

	baseURL := h.cfg.Server.BaseURL

	for _, r := range recipients {
		downloadURL := baseURL + "/dl/" + r.DownloadToken
		subject := "Files available: " + t.Title
		bodyHTML := buildAvailableHTML(t.SenderName, t.Title, t.Message, downloadURL)
		bodyText := buildAvailableText(t.SenderName, t.Title, t.Message, downloadURL)

		if err := h.stores.Mail.Enqueue(nil, r.Email, subject, bodyHTML, bodyText); err != nil {
			slog.Error("tus: enqueue recipient mail", "to", r.Email, "error", err)
		}

		if err := h.stores.Transfers.MarkRecipientNotified(r.ID); err != nil {
			slog.Error("tus: mark recipient notified", "recipient_id", r.ID, "error", err)
		}
	}

	// Sender confirmation
	subject := "Transfer sent: " + t.Title
	bodyHTML := buildConfirmHTML(t.Title, len(recipients))
	bodyText := buildConfirmText(t.Title, len(recipients))
	if err := h.stores.Mail.Enqueue(nil, t.SenderEmail, subject, bodyHTML, bodyText); err != nil {
		slog.Error("tus: enqueue sender confirmation", "to", t.SenderEmail, "error", err)
	}

	return nil
}

// ── Post-PATCH activity ───────────────────────────────────────────────────────

func (h *Handler) recordPatchActivity(r *http.Request) {
	tusID := filepath.Base(r.URL.Path)
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

func rejectWith(status int, msg string) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	return tusd.HTTPResponse{
		StatusCode: status,
		Body:       msg,
	}, tusd.FileInfoChanges{}, fmt.Errorf("%s", msg)
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
// Placeholder implementations — replace with template rendering in internal/mail/.

func buildAvailableHTML(senderName, title, message, downloadURL string) string {
	return fmt.Sprintf(`<p><strong>%s</strong> has shared files with you.</p>
<p><strong>%s</strong></p>
<p>%s</p>
<p><a href="%s">Download files</a></p>`,
		senderName, title, message, downloadURL)
}

func buildAvailableText(senderName, title, message, downloadURL string) string {
	return fmt.Sprintf("%s has shared files with you.\n\n%s\n%s\n\nDownload: %s",
		senderName, title, message, downloadURL)
}

func buildConfirmHTML(title string, recipientCount int) string {
	return fmt.Sprintf(`<p>Your transfer <strong>%s</strong> has been sent to %s.</p>`,
		title, strconv.Itoa(recipientCount)+" recipient(s)")
}

func buildConfirmText(title string, recipientCount int) string {
	return fmt.Sprintf("Your transfer '%s' has been sent to %d recipient(s).",
		title, recipientCount)
}
