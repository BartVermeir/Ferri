package tus

// Tests for the PreUploadCreateCallback validation: an Upload-Length above
// limits.max_upload_bytes must be rejected (413) for both the transfer and
// the upload-request TUS flows, and a valid-size upload must be accepted
// and produce the metadata the completion hooks rely on.

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
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
	cfg.Limits.MinFreeBytes = 1      // the default 50 GB is more than a CI runner has free

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

// A transfer takes at most twice the files /send announced (slack for
// tus-js-client re-POSTs); beyond that the TUS endpoint refuses (audit M3).
func TestPreUploadCreate_TransferBeyondAnnouncedRejected(t *testing.T) {
	h, _, stores := newTestHandler(t)
	result, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:         "Two files",
		SenderEmail:   "alice@example.com",
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		Recipients:    []string{"bob@example.com"},
		ExpectedFiles: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := func() (tusd.HTTPResponse, error) {
		resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
			Size:     10,
			MetaData: tusd.MetaData{"transfer_id": result.TransferID, "filename": "a.bin"},
		}})
		return resp, err
	}

	for i := 1; i <= 4; i++ {
		if resp, err := create(); err != nil {
			t.Fatalf("upload %d of the allowed 4 refused: %v (status %d)", i, err, resp.StatusCode)
		}
	}
	resp, err := create()
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("5th upload: status = %d, err = %v, want 403", resp.StatusCode, err)
	}
}

// An upload that would leave less than min_free_bytes on storage is refused
// with 507 before a file row exists, for transfers and requests alike (M3).
func TestPreUploadCreate_StorageFullRejected(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	cfg.Limits.MinFreeBytes = 1 << 62 // more than any disk has
	transferID := mustCreatePendingTransfer(t, stores)
	uploadToken := mustCreateOpenRequest(t, stores)

	for name, meta := range map[string]tusd.MetaData{
		"transfer": {"transfer_id": transferID, "filename": "a.bin"},
		"request":  {"upload_request_token": uploadToken, "filename": "a.bin"},
	} {
		resp, changes, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{Size: 10, MetaData: meta}})
		if err == nil || resp.StatusCode != http.StatusInsufficientStorage {
			t.Fatalf("%s: status = %d, err = %v, want 507", name, resp.StatusCode, err)
		}
		if changes.MetaData[metaKeyFileID] != "" {
			t.Fatalf("%s: a file row was created for a refused upload", name)
		}
	}
	if count, _, err := stores.Transfers.CountFiles(transferID); err != nil || count != 0 {
		t.Fatalf("transfer file rows = %d (err %v), want 0", count, err)
	}
}

// noStatfsBackend is local storage whose free space cannot be read, like an
// SMB server that does not answer the query.
type noStatfsBackend struct{ *storage.LocalBackend }

func (noStatfsBackend) FreeSpace() (uint64, error) { return 0, errors.New("statfs not supported") }

// When the free space is unknown the upload goes ahead: refusing would stop
// every upload on such a server (M3).
func TestPreUploadCreate_UnknownFreeSpaceAllowed(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	cfg.Limits.MinFreeBytes = 1 << 62
	h.mgr.Swap(noStatfsBackend{storage.NewLocalBackend(t.TempDir())})
	transferID := mustCreatePendingTransfer(t, stores)

	resp, _, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
		Size:     10,
		MetaData: tusd.MetaData{"transfer_id": transferID, "filename": "a.bin"},
	}})
	if err != nil {
		t.Fatalf("upload refused with unknown free space: %v (status %d)", err, resp.StatusCode)
	}
}

// Through the real TUS endpoint a refusal must reach the browser with its own
// status and text. Before, tusd turned every hook refusal into a 500, which
// tus-js-client retried and then showed as "unexpected response" (M3).
func TestTUSPost_RefusalReachesClient(t *testing.T) {
	h, cfg, stores := newTestHandler(t)
	cfg.Limits.MinFreeBytes = 1 << 62
	transferID := mustCreatePendingTransfer(t, stores)

	b64 := func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }
	// main.go mounts the handler behind http.StripPrefix("/tus").
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "10")
	req.Header.Set("Upload-Metadata", "transfer_id "+b64(transferID)+",filename "+b64("a.bin"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != msgStorageFull {
		t.Fatalf("body = %q, want %q", got, msgStorageFull)
	}
}

// DEC-035: a file from a folder is stored with its path, cleaned: "../" and
// absolute parts cannot reach the ZIP a recipient downloads later.
func TestPreUploadCreate_StoresCleanFolderPath(t *testing.T) {
	h, _, stores := newTestHandler(t)
	transferID := mustCreatePendingTransfer(t, stores)
	for name, want := range map[string]string{
		"Series/day1/img001.jpg": "Series/day1/img001.jpg",
		"../../etc/passwd":      "etc/passwd",
		`\\server\share\x.mov`:  "server/share/x.mov",
	} {
		_, changes, err := h.preUploadCreate(tusd.HookEvent{Upload: tusd.FileInfo{
			Size: 10, MetaData: tusd.MetaData{"transfer_id": transferID, "filename": name},
		}})
		if err != nil {
			t.Fatalf("%q refused: %v", name, err)
		}
		f, err := stores.Transfers.GetFileByID(changes.MetaData[metaKeyFileID])
		if err != nil || f == nil {
			t.Fatal(err)
		}
		if f.OriginalName != want {
			t.Errorf("stored name for %q = %q, want %q", name, f.OriginalName, want)
		}
	}
}
