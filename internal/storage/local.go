package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// abs resolves a relative storage path to an absolute one and enforces that it
// stays within the backend root. filepath.Join already collapses ".." segments;
// the prefix check rejects any path that would still escape root — a defensive
// invariant so no future caller can introduce path traversal, even though all
// current paths are server-generated.
func (b *LocalBackend) abs(path string) (string, error) {
	clean := filepath.Join(b.root, filepath.FromSlash(path))
	if clean != b.root && !strings.HasPrefix(clean, b.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid storage path %q: escapes root", path)
	}
	return clean, nil
}

// Open opens a file for reading.
func (b *LocalBackend) Open(path string) (io.ReadSeekCloser, error) {
	abs, err := b.abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat returns FileInfo for the given relative path.
func (b *LocalBackend) Stat(path string) (os.FileInfo, error) {
	abs, err := b.abs(path)
	if err != nil {
		return nil, err
	}
	return os.Stat(abs)
}

// Remove deletes a single file. Returns nil if the file does not exist.
func (b *LocalBackend) Remove(path string) error {
	abs, err := b.abs(path)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveAll deletes a directory and all its contents.
func (b *LocalBackend) RemoveAll(path string) error {
	abs, err := b.abs(path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// MkdirAll creates a directory path and all parents.
func (b *LocalBackend) MkdirAll(path string) error {
	abs, err := b.abs(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
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
