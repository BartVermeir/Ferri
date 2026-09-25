package jobs

// End-to-end test for the cleanup job: an expired transfer past its grace
// period must have its files physically removed from storage and its DB
// rows marked deleted (MarkFilesDeleted + SoftDelete), run against a real
// in-memory SQLite DB and a real local storage backend.

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

// ── Physical removal of the flat TUS files (audit H1) ────────────────────────

type purgeFixture struct {
	t      *testing.T
	d      *sql.DB
	stores *store.Stores
	root   string
	s      *Scheduler
}

func newPurgeFixture(t *testing.T) *purgeFixture {
	t.Helper()
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
	return &purgeFixture{t: t, d: d, stores: stores, root: root, s: NewScheduler(newCleanupTestConfig(), stores, mgr)}
}

// writeTUS puts a flat TUS upload (<id> + <id>.info) on storage, the layout
// both tusd's filestore and Ferri's SMB store use.
func (f *purgeFixture) writeTUS(tusID string) {
	f.t.Helper()
	for _, name := range []string{tusID, tusID + ".info"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("partial"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *purgeFixture) assertGone(tusID string) {
	f.t.Helper()
	for _, name := range []string{tusID, tusID + ".info"} {
		if _, err := os.Stat(filepath.Join(f.root, name)); !os.IsNotExist(err) {
			f.t.Errorf("%s still on storage (stat err = %v)", name, err)
		}
	}
}

// tusID returns the file row's tus_upload_id, "" when NULL.
func (f *purgeFixture) tusID(table, fileID string) string {
	f.t.Helper()
	var id sql.NullString
	if err := f.d.QueryRow(`SELECT tus_upload_id FROM `+table+` WHERE id = ?`, fileID).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id.String
}

func (f *purgeFixture) transferFile(fileID, tusID string) string {
	f.t.Helper()
	res, err := f.stores.Transfers.Create(store.CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.stores.Transfers.CreateFileRow(fileID, res.TransferID, "big.mov", "transfers/"+res.TransferID+"/"+fileID, 1000); err != nil {
		f.t.Fatal(err)
	}
	if err := f.stores.Transfers.SetTUSUploadID(fileID, tusID); err != nil {
		f.t.Fatal(err)
	}
	f.writeTUS(tusID)
	return res.TransferID
}

func (f *purgeFixture) requestFile(fileID, tusID string) {
	f.t.Helper()
	requestID, _, err := f.stores.Requests.Create(store.CreateRequestInput{
		RequesterEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.stores.Requests.CreateFileRow(fileID, requestID, "big.mov", "requests/"+requestID+"/"+fileID, 1000); err != nil {
		f.t.Fatal(err)
	}
	if err := f.stores.Requests.SetTUSUploadID(fileID, tusID); err != nil {
		f.t.Fatal(err)
	}
	f.writeTUS(tusID)
}

// Stalled uploads used to lose only their (non-existent) storage_path; the
// real data at <tus_upload_id> stayed on storage forever.
func TestCleanupStalled_RemovesFlatTUSFiles(t *testing.T) {
	f := newPurgeFixture(t)
	f.transferFile("tf", "tus-transfer")
	f.requestFile("rf", "tus-request")
	for _, table := range []string{"files", "upload_request_files"} {
		if _, err := f.d.Exec(`UPDATE ` + table + ` SET created_at = unixepoch() - 72*3600`); err != nil {
			t.Fatal(err)
		}
	}

	f.s.cleanupStalled()

	f.assertGone("tus-transfer")
	f.assertGone("tus-request")
	if id := f.tusID("files", "tf"); id != "" {
		t.Errorf("transfer file tus_upload_id = %q, want NULL (purged)", id)
	}
	if id := f.tusID("upload_request_files", "rf"); id != "" {
		t.Errorf("request file tus_upload_id = %q, want NULL (purged)", id)
	}
}

// Expired transfers go through the same removal.
func TestCleanupJob_ExpiredTransferRemovesFlatTUSFile(t *testing.T) {
	f := newPurgeFixture(t)
	transferID := f.transferFile("tf", "tus-transfer")
	if err := f.stores.Transfers.SetFileComplete("tf", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`UPDATE transfers SET status = 'expired', expired_at = unixepoch() - 1000000 WHERE id = ?`, transferID); err != nil {
		t.Fatal(err)
	}

	f.s.runCleanupJob()

	f.assertGone("tus-transfer")
	if id := f.tusID("files", "tf"); id != "" {
		t.Errorf("tus_upload_id = %q, want NULL (purged)", id)
	}
}

// Rows deleted before the fix still point at data on storage. The next
// cleanup run must find and remove it — on SMB nothing else would.
func TestCleanupJob_PurgesLeftoversFromBeforeTheFix(t *testing.T) {
	f := newPurgeFixture(t)
	f.transferFile("tf", "tus-transfer")
	f.requestFile("rf", "tus-request")
	if _, err := f.d.Exec(`UPDATE files SET status = 'deleted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`UPDATE upload_request_files SET status = 'deleted'`); err != nil {
		t.Fatal(err)
	}

	f.s.runCleanupJob()

	f.assertGone("tus-transfer")
	f.assertGone("tus-request")
	left, err := f.stores.Files.ListUnpurged()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("still unpurged after cleanup: %+v", left)
	}
}

// A removal that fails keeps its tus_upload_id and is retried next run.
func TestCleanupJob_RetriesFailedRemoval(t *testing.T) {
	f := newPurgeFixture(t)
	f.transferFile("tf", "tus-transfer")
	if _, err := f.d.Exec(`UPDATE files SET status = 'deleted'`); err != nil {
		t.Fatal(err)
	}
	// Replace the data file with a non-empty directory: Remove fails on it.
	blocker := filepath.Join(f.root, "tus-transfer")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(blocker, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}

	f.s.runCleanupJob()
	if id := f.tusID("files", "tf"); id != "tus-transfer" {
		t.Fatalf("after failed removal tus_upload_id = %q, want it kept for retry", id)
	}

	// The obstacle goes away (an empty directory is removable); next run succeeds.
	if err := os.Remove(filepath.Join(blocker, "inside")); err != nil {
		t.Fatal(err)
	}
	f.s.runCleanupJob()
	f.assertGone("tus-transfer")
	if id := f.tusID("files", "tf"); id != "" {
		t.Fatalf("after successful retry tus_upload_id = %q, want NULL", id)
	}
}

// ── Expiry of pending transfers (audit L7, needed by the H2 fix) ─────────────

// An abandoned (pending) transfer expires like any other, but its sender gets
// no "who downloaded what" summary: no link was ever sent. A transfer that did
// go live still gets one.
func TestExpiryJob_PendingExpiresWithoutSummary(t *testing.T) {
	f := newPurgeFixture(t)
	if err := f.stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	mk := func(title string) string {
		res, err := f.stores.Transfers.Create(store.CreateTransferInput{
			Title: title, SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
			Recipients: []string{"bob@example.com"}, NotifyRecipients: true, ExpectedFiles: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.TransferID
	}
	pending, live := mk("never sent"), mk("was live")
	if err := f.stores.Transfers.CreateFileRow("lf", live, "a.mov", "p", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.stores.Transfers.SetFileComplete("lf", 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.stores.Transfers.TryActivate(live); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	if _, err := f.d.Exec(`UPDATE transfers SET expires_at = unixepoch() - 60`); err != nil {
		t.Fatal(err)
	}

	f.s.runExpiryJob()

	for _, id := range []string{pending, live} {
		tr, err := f.stores.Transfers.GetByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Status != "expired" {
			t.Errorf("transfer %q status = %q, want expired", tr.Title, tr.Status)
		}
	}
	items, err := f.stores.Mail.FetchPending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Subject, "was live") {
		var subjects []string
		for _, it := range items {
			subjects = append(subjects, it.Subject)
		}
		t.Fatalf("summaries = %q, want exactly one, for the transfer that was live", subjects)
	}
}

// ── Expiry summary content ───────────────────────────────────────────────────

// The summary says per recipient which files they downloaded and when, and
// which not. It used to list bare timestamps ("who downloaded what" without
// the what), counted a ZIP of 3 files as 3 downloads, and showed a recipient
// who took 2 of 3 files the same as one who took everything.
func TestExpirySummary_WhoDownloadedWhat(t *testing.T) {
	f := newPurgeFixture(t)
	if err := f.stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	res, err := f.stores.Transfers.Create(store.CreateTransferInput{
		Title: "Project", SenderName: "Alice", SenderEmail: "alice@example.com",
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com", "carol@example.com", "dave@example.com"},
		Files: []store.CreateFileInput{
			{OriginalName: "a.mov", StoragePath: "p/a", SizeBytes: 1},
			{OriginalName: "b.mov", StoragePath: "p/b", SizeBytes: 1},
			{OriginalName: "c.mov", StoragePath: "p/c", SizeBytes: 1},
		},
		NotifyRecipients: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID := map[string]string{}
	for i, name := range []string{"a.mov", "b.mov", "c.mov"} {
		fileID[name] = res.Files[i].FileID
		if err := f.stores.Transfers.SetFileComplete(res.Files[i].FileID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := f.stores.Transfers.TryActivate(res.TransferID); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	// A dead row from a lost TUS create: never part of the transfer.
	if err := f.stores.Transfers.CreateFileRow("ghost", res.TransferID, "ghost.mov", "p/ghost", 1); err != nil {
		t.Fatal(err)
	}
	recipient := map[string]string{}
	for _, r := range res.Recipients {
		recipient[r.Email] = r.RecipientID
	}

	day1 := time.Date(2026, 9, 11, 13, 14, 0, 0, time.UTC).Unix()
	day2 := time.Date(2026, 9, 12, 9, 2, 0, 0, time.UTC).Unix()
	download := func(email, file string, at int64) {
		t.Helper()
		id, _, err := f.stores.Downloads.RecordDownload(recipient[email], fileID[file], file, "", "", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.Exec(`UPDATE download_events SET downloaded_at = ? WHERE id = ?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	download("bob@example.com", "a.mov", day1)
	download("bob@example.com", "a.mov", day2)
	download("bob@example.com", "b.mov", day1)
	download("bob@example.com", "b.mov", day1+20) // same minute
	for _, file := range []string{"a.mov", "b.mov", "c.mov"} {
		download("carol@example.com", file, day1) // one ZIP
	}

	if _, err := f.d.Exec(`UPDATE transfers SET expires_at = unixepoch() - 60`); err != nil {
		t.Fatal(err)
	}
	f.s.runExpiryJob()

	items, err := f.stores.Mail.FetchPending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d mails, want the one summary", len(items))
	}
	text, htmlBody := items[0].BodyText, items[0].BodyHTML

	for _, want := range []string{
		"bob@example.com: 2 of 3 files\n  • a.mov: 11 Sep 13:14, 12 Sep 09:02\n  • b.mov: 11 Sep 13:14 (2×)\n  Not downloaded: c.mov\n",
		"carol@example.com: all 3 files\n  • All files: 11 Sep 13:14\n",
		"dave@example.com: nothing downloaded\n\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text summary misses:\n%s\n--- got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ghost.mov") {
		t.Error("the dead upload row shows up in the summary")
	}
	for _, want := range []string{"2 of 3 files", "All files", "Not downloaded: c.mov", "nothing downloaded", "11 Sep 13:14 (2×)"} {
		if !strings.Contains(htmlBody, want) {
			t.Errorf("HTML summary misses %q", want)
		}
	}
}

// File names come from the uploader's browser and must be escaped in HTML.
func TestExpirySummary_EscapesNames(t *testing.T) {
	e := expirySummary{
		Loc:      time.UTC,
		Transfer: store.Transfer{NotifyRecipients: true},
		Files:    []store.File{{ID: "f1", OriginalName: "<b>x</b>.mov", Status: "complete"}},
		History: []store.RecipientHistory{{
			Email:  "bob@example.com",
			Events: []store.DownloadEvent{{FileID: sql.NullString{String: "f1", Valid: true}, OriginalName: "<b>x</b>.mov", DownloadedAt: 1}},
		}},
	}
	out := buildExpirySummaryHTML(e)
	if strings.Contains(out, "<b>x</b>") || !strings.Contains(out, "&lt;b&gt;x&lt;/b&gt;.mov") {
		t.Fatalf("file name not escaped in HTML summary:\n%s", out)
	}
}
