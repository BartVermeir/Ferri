package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// LocalBackend stores files on the local filesystem.
// It wraps standard os.* calls and tusd's built-in filestore.
type LocalBackend struct {
	root  string // absolute path, e.g. "/data/storage"
	store tusd.DataStore
}

// NewLocalBackend creates a LocalBackend rooted at root.
// root must be an absolute path. The directory is created if it does not exist.
func NewLocalBackend(root string) *LocalBackend {
	fs := filestore.New(root)
	return &LocalBackend{root: root, store: fs}
}

func (b *LocalBackend) abs(path string) string {
	return filepath.Join(b.root, filepath.FromSlash(path))
}

// Open opens a file for reading.
func (b *LocalBackend) Open(path string) (io.ReadSeekCloser, error) {
	f, err := os.Open(b.abs(path))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat returns FileInfo for the given relative path.
func (b *LocalBackend) Stat(path string) (os.FileInfo, error) {
	return os.Stat(b.abs(path))
}

// Remove deletes a single file. Returns nil if the file does not exist.
func (b *LocalBackend) Remove(path string) error {
	err := os.Remove(b.abs(path))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RemoveAll deletes a directory and all its contents.
func (b *LocalBackend) RemoveAll(path string) error {
	err := os.RemoveAll(b.abs(path))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// MkdirAll creates a directory path and all parents.
func (b *LocalBackend) MkdirAll(path string) error {
	return os.MkdirAll(b.abs(path), 0o755)
}

// TUSStore returns the tusd.DataStore for this backend.
func (b *LocalBackend) TUSStore() tusd.DataStore {
	return b.store
}

// TestConnection checks that the root directory is readable and writable.
func (b *LocalBackend) TestConnection() error {
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return fmt.Errorf("cannot create storage root: %w", err)
	}
	probe := filepath.Join(b.root, ".ferri-probe")
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("storage not writable: %w", err)
	}
	f.Close()
	os.Remove(probe)
	return nil
}

// Type returns "local".
func (b *LocalBackend) Type() string { return "local" }

// Close is a no-op for local storage.
func (b *LocalBackend) Close() error { return nil }
