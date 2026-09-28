package handler

// Shared test helpers for the handler package's HTTP-level tests
// (download_test.go, upload_test.go, admin_test.go). Builds a real config,
// a fresh in-memory migrated SQLite DB, and a local storage backend rooted
// at a temp dir — no mocks, matching the rest of this codebase's test style.

import (
	"database/sql"
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

// newTestConfig returns a Config that satisfies everything the handlers under
// test read, without going through config.Load/validate (which is
// package-private to internal/config).
func newTestConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Server.BaseURL = "http://example.com"
	cfg.Server.Location = time.UTC
	cfg.SMTP.Host = "smtp.example.com"
	cfg.Admin.Token = strings.Repeat("x", 32)
	return cfg
}

// newTestDB opens a fresh in-memory SQLite DB and applies all migrations.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return d
}

// newTestStores wires a Stores against a fresh in-memory DB.
func newTestStores(t *testing.T) *store.Stores {
	t.Helper()
	return store.New(newTestDB(t))
}

// newTestManager returns a storage.Manager backed by a local temp directory,
// along with that directory's absolute path (for writing fixture files).
func newTestManager(t *testing.T) (*storage.Manager, string) {
	t.Helper()
	root := t.TempDir()
	return storage.NewManager(storage.NewLocalBackend(root)), root
}

// writeStorageFile creates a file with the given content at a path relative
// to the storage root, creating parent directories as needed.
func writeStorageFile(t *testing.T, root, relPath string, content []byte) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
