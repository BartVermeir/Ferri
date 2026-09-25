package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/store"
)

// Audit L8: orphan cleanup deleted every file in the storage folder that the
// database did not know, a database file included, and never recognised a
// request's upload by its .info (it looked for the wrong metadata key).
func TestAdminOrphanClean_OnlyUnreferencedTUSUploads(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	root := t.TempDir()
	cfg.Storage.Path = root
	requestID, _, err := stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orphan := strings.Repeat("a", 32)
	requestUpload := strings.Repeat("b", 32)
	write(orphan, "x")
	write(orphan+".info", `{"MetaData":{"transfer_id":"gone"}}`)
	write(requestUpload, "x")
	write(requestUpload+".info", `{"MetaData":{"context":"request","request_id":"`+requestID+`"}}`)
	write("notes.db", "not ours")
	write(".ferri-probe", "")

	rr := httptest.NewRecorder()
	AdminOrphanClean(cfg, stores).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/orphans/clean", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}

	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }
	if exists(orphan) {
		t.Error("an unreferenced TUS upload was kept")
	}
	if !exists(requestUpload) {
		t.Error("a request's upload was deleted: its .info names a live request")
	}
	for _, name := range []string{"notes.db", ".ferri-probe"} {
		if !exists(name) {
			t.Errorf("%s was deleted: not a TUS upload", name)
		}
	}
}

func TestAdminOrphanClean_RefusesFolderWithDatabase(t *testing.T) {
	cfg := newTestConfig()
	root := t.TempDir()
	cfg.Storage.Path = root
	cfg.DB.Path = filepath.Join(root, "app.db")
	victim := filepath.Join(root, strings.Repeat("c", 32))
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	AdminOrphanClean(cfg, newTestStores(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/orphans/clean", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("files were deleted although the scan was refused")
	}
}

func TestAdminStorageSave_RefusesFolderWithDatabase(t *testing.T) {
	cfg := newTestConfig()
	cfg.DB.Path = "/data/app.db"
	stores := newTestStores(t)
	form := url.Values{"storage.type": {"local"}, "storage.local_path": {"/data"}}
	rr := postForm(AdminStorageSave(cfg, stores, mustManager(t)), "/admin/settings/storage", form)
	if !strings.Contains(rr.Header().Get("Location"), "storage_error=") {
		t.Fatalf("saving /data as storage folder was accepted (Location %q)", rr.Header().Get("Location"))
	}
	if p := stores.Settings.Get().LocalPath; p == "/data" {
		t.Fatal("the folder holding the database was saved as storage path")
	}
}

func TestPathContainsDB(t *testing.T) {
	cases := []struct {
		dir, db string
		want    bool
	}{
		{"/data", "/data/app.db", true},
		{"/", "/data/app.db", true},
		{"/data/storage", "/data/app.db", false},
		{"/data/storage/", "/data/app.db", false},
		{"/database", "/data/app.db", false},
	}
	for _, c := range cases {
		if got := pathContainsDB(c.dir, c.db); got != c.want {
			t.Errorf("pathContainsDB(%q, %q) = %v, want %v", c.dir, c.db, got, c.want)
		}
	}
}
