package storage

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		abs := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveUpload_RemovesAllFourPaths(t *testing.T) {
	root := t.TempDir()
	m := NewManager(NewLocalBackend(root))
	paths := []string{"transfers/t1/f1", "transfers/t1/f1.info", "tus-1", "tus-1.info"}
	writeFiles(t, root, paths...)

	if err := m.RemoveUpload("transfers/t1/f1", "tus-1"); err != nil {
		t.Fatalf("RemoveUpload: %v", err)
	}
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s still exists (stat err = %v)", p, err)
		}
	}
}

// The data of a TUS upload lives at the flat <tus_upload_id>; storage_path
// never exists for those. Missing paths must count as removed, or every
// purge would fail and be retried forever.
func TestRemoveUpload_MissingPathsAreNotAnError(t *testing.T) {
	root := t.TempDir()
	m := NewManager(NewLocalBackend(root))
	writeFiles(t, root, "tus-1") // no .info, no storage_path

	if err := m.RemoveUpload("transfers/t1/f1", "tus-1"); err != nil {
		t.Fatalf("RemoveUpload: %v", err)
	}
	if err := m.RemoveUpload("transfers/t1/f1", "tus-1"); err != nil {
		t.Fatalf("second RemoveUpload should be a no-op: %v", err)
	}
}

// Remove("") resolves to the storage root. Empty arguments must be skipped.
func TestRemoveUpload_EmptyArgumentsLeaveRootAlone(t *testing.T) {
	root := t.TempDir()
	m := NewManager(NewLocalBackend(root))
	writeFiles(t, root, "keep-me")

	if err := m.RemoveUpload("", ""); err != nil {
		t.Fatalf("RemoveUpload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me")); err != nil {
		t.Fatalf("storage root was touched: %v", err)
	}
}

func TestRemoveUpload_ReportsFailure(t *testing.T) {
	root := t.TempDir()
	m := NewManager(NewLocalBackend(root))
	// A non-empty directory at the TUS path cannot be removed with Remove.
	writeFiles(t, root, "tus-1/blocker")

	if err := m.RemoveUpload("", "tus-1"); err == nil {
		t.Fatal("expected an error for a path that could not be removed")
	}
}

func TestLocalBackend_FreeSpace(t *testing.T) {
	free, err := NewLocalBackend(t.TempDir()).FreeSpace()
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free == 0 {
		t.Fatal("FreeSpace = 0 on a writable temp dir")
	}
}

// closeCounter is local storage that records being closed.
type closeCounter struct {
	*LocalBackend
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

// Audit L13: swapping the storage used to close the old backend at once,
// breaking off downloads still reading from it. Now it closes when the last
// open file is closed, or immediately when nothing uses it.
func TestSwap_ClosesOldBackendAfterLastUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := &closeCounter{LocalBackend: NewLocalBackend(dir)}
	m := NewManager(old)

	f, err := m.Open("f")
	if err != nil {
		t.Fatal(err)
	}
	next := &closeCounter{LocalBackend: NewLocalBackend(t.TempDir())}
	m.Swap(next)
	if old.closed != 0 {
		t.Fatal("old backend closed while a file on it is still open")
	}
	if got, _ := io.ReadAll(f); string(got) != "data" {
		t.Fatalf("read after swap = %q", got)
	}
	f.Close()
	if old.closed != 1 {
		t.Fatalf("old backend closed %d times after its last file closed, want 1", old.closed)
	}

	m.Swap(&closeCounter{LocalBackend: NewLocalBackend(t.TempDir())})
	if next.closed != 1 {
		t.Fatalf("idle backend closed %d times on swap, want 1 (right away)", next.closed)
	}
}
