package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
)

// Backend abstracts all file storage operations.
//
// All paths passed to Backend methods are relative to the backend's configured
// root (e.g. cfg.Storage.Path for local, smb_base_path on the share for SMB).
// The backend prepends its own root internally — callers never deal with
// absolute paths or host-specific path separators.
type Backend interface {
	// Open opens a file for reading.
	// The returned ReadSeekCloser is suitable for http.ServeContent.
	// The caller is responsible for closing it.
	Open(path string) (io.ReadSeekCloser, error)

	// Stat returns FileInfo for the given relative path.
	Stat(path string) (os.FileInfo, error)

	// Remove deletes a single file. Returns nil if the file does not exist.
	Remove(path string) error

	// RemoveAll deletes a directory and all its contents.
	// Returns nil if the path does not exist.
	RemoveAll(path string) error

	// MkdirAll creates a directory path and all parents.
	MkdirAll(path string) error

	// TUSStore returns a tusd.DataStore backed by this storage backend.
	// The store writes flat files (<root>/<uuid> and <root>/<uuid>.info).
	TUSStore() tusd.DataStore

	// TestConnection verifies the backend is accessible and writable.
	TestConnection() error

	// Type returns "local" or "smb".
	Type() string

	// Close releases any held resources (network connections, file handles).
	Close() error
}

// Manager wraps a Backend and allows atomic hot-swap without interrupting
// read operations. All storage calls should go through the Manager.
type Manager struct {
	mu      sync.RWMutex
	backend Backend
}

// NewManager creates a Manager with the given initial backend.
func NewManager(b Backend) *Manager {
	return &Manager{backend: b}
}

// Open opens a file for reading.
func (m *Manager) Open(path string) (io.ReadSeekCloser, error) {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.Open(path)
}

// Stat returns file info.
func (m *Manager) Stat(path string) (os.FileInfo, error) {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.Stat(path)
}

// Remove deletes a file.
func (m *Manager) Remove(path string) error {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.Remove(path)
}

// RemoveAll deletes a directory and all its contents.
func (m *Manager) RemoveAll(path string) error {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.RemoveAll(path)
}

// MkdirAll creates a directory path and all parents.
func (m *Manager) MkdirAll(path string) error {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.MkdirAll(path)
}

// TUSDataStore returns a tusd.DataStore that always delegates to the currently
// active backend. Replacing the backend via Swap does not invalidate the store —
// subsequent TUS operations will use the new backend automatically.
//
// Note: uploads already in progress when Swap is called will fail if the
// new backend does not have their existing upload data. This is expected
// behaviour and documented as a known limitation.
func (m *Manager) TUSDataStore() tusd.DataStore {
	return &managerTUSStore{m: m}
}

// Type returns the type string of the active backend.
func (m *Manager) Type() string {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.Type()
}

// TestConnection delegates to the active backend's TestConnection.
func (m *Manager) TestConnection() error {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.TestConnection()
}

// Swap replaces the active backend. The old backend is closed gracefully.
// Returns an error only if the old backend fails to close; the swap itself
// always succeeds.
func (m *Manager) Swap(newBackend Backend) {
	m.mu.Lock()
	old := m.backend
	m.backend = newBackend
	m.mu.Unlock()

	if old != nil {
		if err := old.Close(); err != nil {
			slog.Warn("storage: close old backend on swap", "error", err)
		}
	}
	slog.Info("storage: backend swapped", "type", newBackend.Type())
}

// Close closes the active backend.
func (m *Manager) Close() error {
	m.mu.Lock()
	b := m.backend
	m.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Close()
}

// FromSettings creates a Backend from the current runtime settings.
// adminToken is used to derive the encryption key for the SMB password.
func FromSettings(settings *store.Settings, cfg *config.Config, adminToken string) (Backend, error) {
	switch settings.StorageType {
	case "smb":
		if settings.SMBHost == "" || settings.SMBShare == "" {
			return nil, fmt.Errorf("SMB storage requires host and share name to be configured")
		}
		password := ""
		if settings.SMBPasswordEncrypted != "" {
			key := DeriveKey(adminToken)
			decrypted, err := Decrypt(key, settings.SMBPasswordEncrypted)
			if err != nil {
				return nil, fmt.Errorf("decrypt SMB password: %w", err)
			}
			password = decrypted
		}
		return NewSMBBackend(SMBConfig{
			Host:     settings.SMBHost,
			Share:    settings.SMBShare,
			BasePath: settings.SMBBasePath,
			Username: settings.SMBUsername,
			Password: password,
			Domain:   settings.SMBDomain,
		})
	default:
		return NewLocalBackend(cfg.Storage.Path), nil
	}
}

// ── managerTUSStore ───────────────────────────────────────────────────────────

// managerTUSStore implements tusd.DataStore by delegating to whatever backend
// is currently active in the Manager. Created once at startup via TUSDataStore().
type managerTUSStore struct {
	m *Manager
}

func (d *managerTUSStore) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	d.m.mu.RLock()
	b := d.m.backend
	d.m.mu.RUnlock()
	return b.TUSStore().NewUpload(ctx, info)
}

func (d *managerTUSStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	d.m.mu.RLock()
	b := d.m.backend
	d.m.mu.RUnlock()
	return b.TUSStore().GetUpload(ctx, id)
}
