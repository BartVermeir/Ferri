package jobs

// End-to-end test for the cleanup job: an expired transfer past its grace
// period must have its files physically removed from storage and its DB
// rows marked deleted (MarkFilesDeleted + SoftDelete), run against a real
// in-memory SQLite DB and a real local storage backend.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/db"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

func newCleanupTestConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Server.BaseURL = "http://example.com"
	cfg.Server.Location = time.UTC
	cfg.SMTP.Host = "smtp.example.com"
	cfg.Admin.Token = strings.Repeat("x", 32)
	cfg.Jobs.CleanupGraceHours = 24
	return cfg
}

func TestCleanupJob_RemovesExpiredTransferFilesAndMarksDeleted(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.New(d)

	root := t.TempDir()
	mgr := storage.NewManager(storage.NewLocalBackend(root))

	storagePath := "transfers/fixed/payload.bin"
	content := []byte("payload bytes")

	result, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:       "Expired transfer",
		SenderEmail: "alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"bob@example.com"},
		Files:       []store.CreateFileInput{{OriginalName: "payload.bin", StoragePath: storagePath, SizeBytes: int64(len(content))}},
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	fileID := result.Files[0].FileID

	abs := filepath.Join(root, filepath.FromSlash(storagePath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	if err := stores.Transfers.SetFileComplete(fileID, int64(len(content))); err != nil {
		t.Fatalf("set file complete: %v", err)
	}
	if _, err := stores.Transfers.TryActivate(result.TransferID); err != nil {
		t.Fatalf("try activate: %v", err)
	}
	if err := stores.Transfers.SetExpired(result.TransferID); err != nil {
		t.Fatalf("set expired: %v", err)
	}

	// Push expired_at well past the grace period — SetExpired stamps "now",
	// which GetForCleanup would not yet consider due for cleanup.
	if _, err := d.Exec(`UPDATE transfers SET expired_at = unixepoch() - 1000000 WHERE id = ?`, result.TransferID); err != nil {
		t.Fatalf("backdate expired_at: %v", err)
	}

	cfg := newCleanupTestConfig()
	s := NewScheduler(cfg, stores, mgr)
	s.runCleanupJob()

	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed from storage, stat err = %v", err)
	}

	transfer, err := stores.Transfers.GetByID(result.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if transfer == nil || transfer.Status != "deleted" {
		t.Fatalf("transfer status = %+v, want deleted", transfer)
	}

	var fileStatus string
	if err := d.QueryRow(`SELECT status FROM files WHERE id = ?`, fileID).Scan(&fileStatus); err != nil {
		t.Fatal(err)
	}
	if fileStatus != "deleted" {
		t.Fatalf("file status = %q, want deleted", fileStatus)
	}
}

func TestCleanupJob_LeavesNonExpiredTransfersAlone(t *testing.T) {
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

	result, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:       "Active transfer",
		SenderEmail: "alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"bob@example.com"},
		Files:       []store.CreateFileInput{{OriginalName: "f", StoragePath: "transfers/x/f", SizeBytes: 1}},
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}

	cfg := newCleanupTestConfig()
	s := NewScheduler(cfg, stores, mgr)
	s.runCleanupJob()

	transfer, err := stores.Transfers.GetByID(result.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if transfer.Status != "pending" {
		t.Fatalf("transfer status = %q, want unchanged (pending)", transfer.Status)
	}
}
