package storage

import (
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
