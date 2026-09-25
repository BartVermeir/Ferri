package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/store"
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

	// FreeSpace returns the bytes still available to Ferri on the storage.
	FreeSpace() (uint64, error)

	// Type returns "local" or "smb".
	Type() string

	// Close releases any held resources (network connections, file handles).
	Close() error
}

// Manager wraps a Backend and allows atomic hot-swap without interrupting
// read operations. All storage calls should go through the Manager.
//
// A swapped-out backend is closed only once nothing uses it any more: every
// open file and every running upload chunk holds it (use/release). It used
// to be closed at once, which broke off running downloads and uploads on the
// old SMB session (audit L13).
type Manager struct {
	mu      sync.RWMutex
	backend Backend
	users   map[Backend]int  // open files and running upload calls per backend
	retired map[Backend]bool // swapped out, close when users drops to 0
}

// NewManager creates a Manager with the given initial backend.
func NewManager(b Backend) *Manager {
	return &Manager{backend: b, users: map[Backend]int{}, retired: map[Backend]bool{}}
}

// use returns the active backend and registers one user of it; call the
// returned release exactly once when done.
func (m *Manager) use() (Backend, func()) {
	m.mu.Lock()
	b := m.backend
	m.users[b]++
	m.mu.Unlock()
	return b, m.releaser(b)
}

// useBackend registers one more user of b, which may be retired already.
// ok is false when b has been closed: it can no longer be used.
func (m *Manager) useBackend(b Backend) (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b != m.backend && !m.retired[b] {
		return nil, false
	}
	m.users[b]++
	return m.releaser(b), true
}

func (m *Manager) releaser(b Backend) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.users[b]--
			closeNow := m.users[b] <= 0 && m.retired[b]
			if m.users[b] <= 0 {
				delete(m.users, b)
			}
			if closeNow {
				delete(m.retired, b)
			}
			m.mu.Unlock()
			if closeNow {
				closeBackend(b, "last operation on the old backend finished")
			}
		})
	}
}

func closeBackend(b Backend, why string) {
	if err := b.Close(); err != nil {
		slog.Warn("storage: close old backend", "type", b.Type(), "error", err)
		return
	}
	slog.Info("storage: old backend closed", "type", b.Type(), "reason", why)
}

// trackedFile keeps its backend in use until the file is closed.
type trackedFile struct {
	io.ReadSeekCloser
	release func()
}

func (f *trackedFile) Close() error {
	err := f.ReadSeekCloser.Close()
	f.release()
	return err
}

// Open opens a file for reading. The backend stays in use (and open, even
// after a swap) until the returned file is closed.
func (m *Manager) Open(path string) (io.ReadSeekCloser, error) {
	b, release := m.use()
	f, err := b.Open(path)
	if err != nil {
		release()
		return nil, err
	}
	return &trackedFile{ReadSeekCloser: f, release: release}, nil
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

// RemoveUpload removes every file an upload can leave on the backend: the
// logical storage_path (only a real file for legacy local uploads) and the flat
// TUS file <tus_upload_id>, each with its .info sidecar. Empty arguments are
// skipped — Remove("") would resolve to the storage root itself. A path that
// does not exist counts as removed, so calling this twice is safe.
//
// Returns the joined errors of the removals that failed; nil means nothing of
// this upload is left. All deletion code (cleanup, stalled uploads, admin
// delete) goes through here so no caller can forget one of the paths again.
func (m *Manager) RemoveUpload(storagePath, tusUploadID string) error {
	var paths []string
	if storagePath != "" {
		paths = append(paths, storagePath, storagePath+".info")
	}
	if tusUploadID != "" {
		paths = append(paths, tusUploadID, tusUploadID+".info")
	}
	var errs []error
	for _, p := range paths {
		if err := m.Remove(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// PurgeUpload removes one file's data (RemoveUpload) and, only once that fully
// succeeded, clears its tus_upload_id (store.FilesStore.MarkPurged). On
// failure the id stays, which is how the cleanup job knows to retry it
// (jobs.Scheduler.purgeLeftovers). Callers still mark the row deleted either
// way: the download link must stop working now, not after the retry.
func PurgeUpload(m *Manager, files *store.FilesStore, table store.FileTable, fileID, storagePath, tusUploadID string) error {
	if err := m.RemoveUpload(storagePath, tusUploadID); err != nil {
		return err
	}
	if tusUploadID == "" {
		return nil
	}
	if err := files.MarkPurged(table, fileID); err != nil {
		return fmt.Errorf("mark purged: %w", err)
	}
	return nil
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

// FreeSpace delegates to the active backend's FreeSpace.
func (m *Manager) FreeSpace() (uint64, error) {
	m.mu.RLock()
	b := m.backend
	m.mu.RUnlock()
	return b.FreeSpace()
}

// Swap replaces the active backend; new operations use it at once. The old
// backend is closed when its last open file or running upload call is done,
// or right away when nothing uses it.
func (m *Manager) Swap(newBackend Backend) {
	m.mu.Lock()
	old := m.backend
	m.backend = newBackend
	busy := 0
	if old != nil && old != newBackend {
		busy = m.users[old]
		if busy > 0 {
			m.retired[old] = true
		}
	}
	m.mu.Unlock()

	slog.Info("storage: backend swapped", "type", newBackend.Type(), "old_backend_still_in_use", busy)
	if old != nil && old != newBackend && busy == 0 {
		closeBackend(old, "not in use")
	}
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
			decrypted, err := Decrypt(adminToken, settings.SMBPasswordEncrypted)
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
		path := settings.LocalPath
		if path == "" {
			path = cfg.Storage.Path
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("local storage path must be absolute, got %q", path)
		}
		return NewLocalBackend(path), nil
	}
}

// ── managerTUSStore ───────────────────────────────────────────────────────────

// managerTUSStore implements tusd.DataStore by delegating to whatever backend
// is currently active in the Manager. Created once at startup via TUSDataStore().
type managerTUSStore struct {
	m *Manager
}

func (d *managerTUSStore) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	b, release := d.m.use()
	defer release()
	u, err := b.TUSStore().NewUpload(ctx, info)
	if err != nil {
		return nil, err
	}
	return &trackedUpload{Upload: u, m: d.m, b: b}, nil
}

func (d *managerTUSStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	b, release := d.m.use()
	defer release()
	u, err := b.TUSStore().GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	return &trackedUpload{Upload: u, m: d.m, b: b}, nil
}

// trackedUpload keeps its backend in use while a chunk is written or the
// upload is finished, so a storage swap mid-chunk does not close the session
// under it. Once the old backend is closed, the next chunk fails; the browser
// then asks the new backend, which does not know the upload, and starts over.
type trackedUpload struct {
	tusd.Upload
	m *Manager
	b Backend
}

var errBackendClosed = errors.New("storage: this upload's backend was swapped out and closed")

func (u *trackedUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	release, ok := u.m.useBackend(u.b)
	if !ok {
		return 0, errBackendClosed
	}
	defer release()
	return u.Upload.WriteChunk(ctx, offset, src)
}

func (u *trackedUpload) FinishUpload(ctx context.Context) error {
	release, ok := u.m.useBackend(u.b)
	if !ok {
		return errBackendClosed
	}
	defer release()
	return u.Upload.FinishUpload(ctx)
}
