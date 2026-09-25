package tus

// Tests for the PreUploadCreateCallback validation: an Upload-Length above
// limits.max_upload_bytes must be rejected (413) for both the transfer and
// the upload-request TUS flows, and a valid-size upload must be accepted
// and produce the metadata the completion hooks rely on.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/db"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

func newTestHandler(t *testing.T) (*Handler, *config.Config, *store.Stores) {
	t.Helper()

	cfg := config.Defaults()
	cfg.Server.BaseURL = "http://example.com"
	cfg.Server.Location = time.UTC
	cfg.SMTP.Host = "smtp.example.com"
	cfg.Admin.Token = strings.Repeat("x", 32)
	cfg.Limits.MaxUploadBytes = 1024 // small, deterministic limit for these tests

	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	stores := store.New(d)
	mgr := storage.NewManager(storage.NewLocalBackend(t.TempDir()))

	h, err := NewHandler(cfg, stores, mgr)
	if err != nil {
		t.Fatalf("new tus handler: %v", err)
	}
	return h, cfg, stores
}

func mustCreatePendingTransfer(t *testing.T, stores *store.Stores) string {
	t.Helper()
	result, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:       "Test transfer",
		SenderEmail: "alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"bob@example.com"},
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	return result.TransferID
}

func mustCreateOpenRequest(t *testing.T, stores *store.Stores) string {
	t.Helper()
	_, uploadToken, err := stores.Requests.Create(store.CreateRequestInput{
		Title:          "Test request",
		RequesterEmail: "alice@example.com",
		ExpiresAt:      time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	return uploadToken
}

func TestPreUploadCreate_TransferOversizedRejected(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	transferID := mustCreatePendingTransfer(t, stores)

	resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     cfg.Limits.MaxUploadBytes + 1,
		MetaData: tusd.MetaData{"transfer_id": transferID, "filename": "big.mov"},
	}})

	if err == nil {
		t.Fatal("expected an error for an oversized upload")
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestPreUploadCreate_TransferValidSizeAccepted(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	transferID := mustCreatePendingTransfer(t, stores)

	resp, changes, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     cfg.Limits.MaxUploadBytes,
		MetaData: tusd.MetaData{"transfer_id": transferID, "filename": "ok.mov"},
	}})

	if err != nil {
		t.Fatalf("unexpected rejection: %v (resp=%+v)", err, resp)
	}
	if changes.MetaData[metaKeyFileID] == "" {
		t.Fatal("expected a ferri_file_id to be assigned")
	}
	if changes.MetaData["context"] != metaContextTransfer {
		t.Fatalf("context = %q, want %q", changes.MetaData["context"], metaContextTransfer)
	}

	// The file row must exist and be in the 'uploading' state.
	f, err := stores.Transfers.GetFileByID(changes.MetaData[metaKeyFileID])
	if err != nil || f == nil {
		t.Fatalf("file row not created: %v", err)
	}
	if f.Status != "uploading" {
		t.Fatalf("file status = %q, want uploading", f.Status)
	}
}

func TestPreUploadCreate_RequestOversizedRejected(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	uploadToken := mustCreateOpenRequest(t, stores)

	resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     cfg.Limits.MaxUploadBytes + 1,
		MetaData: tusd.MetaData{"upload_request_token": uploadToken, "filename": "big.mov"},
	}})

	if err == nil {
		t.Fatal("expected an error for an oversized upload")
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestPreUploadCreate_RequestValidSizeAccepted(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	uploadToken := mustCreateOpenRequest(t, stores)

	_, changes, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     cfg.Limits.MaxUploadBytes,
		MetaData: tusd.MetaData{"upload_request_token": uploadToken, "filename": "ok.mov"},
	}})

	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if changes.MetaData["context"] != metaContextRequest {
		t.Fatalf("context = %q, want %q", changes.MetaData["context"], metaContextRequest)
	}
}

func TestPreUploadCreate_MissingMetadataRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)

	resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     10,
		MetaData: tusd.MetaData{"filename": "no-context.mov"},
	}})

	if err == nil {
		t.Fatal("expected an error when neither transfer_id nor upload_request_token is present")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Once a transfer is live, recipients have been mailed its file list; the TUS
// endpoint must not let anyone add files to it (403).
func TestPreUploadCreate_ActiveTransferRejected(t *testing.T) {
	h, _, stores := newTestHandler(t)
	transferID := mustCreatePendingTransfer(t, stores)
	if _, err := stores.Transfers.TryActivate(transferID); err != nil {
		t.Fatal(err)
	}

	resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     10,
		MetaData: tusd.MetaData{"transfer_id": transferID, "filename": "late.bin"},
	}})
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, err = %v, want 403 for an active transfer", resp.StatusCode, err)
	}
}
