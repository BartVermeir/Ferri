package store

// Race-safety test for TryActivate: the atomic
// UPDATE ... WHERE (SELECT COUNT(*) ... != 'complete') = 0
// must let exactly one caller win the transition to 'active', even when
// many goroutines race to mark files complete and call TryActivate
// concurrently. See internal/tus/handler.go's completeTransferFile, which
// calls TryActivate after every file completion.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/db"
)

func newRaceTestStores(t *testing.T) *Stores {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(d)
}

func TestTryActivate_ConcurrentOnlyOneWinner(t *testing.T) {
	stores := newRaceTestStores(t)

	const numFiles = 20
	files := make([]CreateFileInput, numFiles)
	for i := range files {
		files[i] = CreateFileInput{OriginalName: "f", StoragePath: "p", SizeBytes: 1}
	}

	result, err := stores.Transfers.Create(CreateTransferInput{
		Title:       "Race transfer",
		SenderEmail: "alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"bob@example.com"},
		Files:       files,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}

	var (
		wg          sync.WaitGroup
		activations int64
	)

	// Each goroutine owns one file: mark it complete, then hammer TryActivate
	// concurrently with every other goroutine racing to do the same for
	// their own file. Only the goroutine whose completion makes ALL files
	// complete should ever observe activated == true, and only once overall.
	for _, f := range result.Files {
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			if err := stores.Transfers.SetFileComplete(fileID, 1); err != nil {
				t.Errorf("set file complete: %v", err)
				return
			}
			for i := 0; i < 5; i++ {
				activated, err := stores.Transfers.TryActivate(result.TransferID)
				if err != nil {
					t.Errorf("try activate: %v", err)
					return
				}
				if activated {
					atomic.AddInt64(&activations, 1)
				}
			}
		}(f.FileID)
	}

	wg.Wait()

	if got := atomic.LoadInt64(&activations); got != 1 {
		t.Fatalf("activations = %d, want exactly 1", got)
	}

	transfer, err := stores.Transfers.GetByID(result.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if transfer.Status != "active" {
		t.Fatalf("transfer status = %q, want active", transfer.Status)
	}
	if !transfer.ActivatedAt.Valid {
		t.Fatal("activated_at not set")
	}
}

// ── expected_files (audit H2) ────────────────────────────────────────────────

func newExpectedTransfer(t *testing.T, stores *Stores, expected int) string {
	t.Helper()
	res, err := stores.Transfers.Create(CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com"}, NotifyRecipients: true,
		ExpectedFiles: expected,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	return res.TransferID
}

func completeFile(t *testing.T, stores *Stores, transferID, fileID string) {
	t.Helper()
	if err := stores.Transfers.CreateFileRow(fileID, transferID, fileID+".mov", "p/"+fileID, 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Transfers.SetFileComplete(fileID, 1); err != nil {
		t.Fatal(err)
	}
}

func mustTryActivate(t *testing.T, stores *Stores, transferID string) bool {
	t.Helper()
	ok, err := stores.Transfers.TryActivate(transferID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func mustValidateForTUS(t *testing.T, stores *Stores, transferID string) bool {
	t.Helper()
	ok, err := stores.Transfers.ValidateForTUS(transferID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// upload.js uploads files one after another: file 2's row does not exist yet
// when file 1 completes. The transfer used to go live right there.
func TestTryActivate_WaitsForAllExpectedFiles(t *testing.T) {
	stores := newRaceTestStores(t)
	id := newExpectedTransfer(t, stores, 2)

	completeFile(t, stores, id, "f1")
	if mustTryActivate(t, stores, id) {
		t.Fatal("activated after 1 of 2 files")
	}
	if !mustValidateForTUS(t, stores, id) {
		t.Fatal("pending transfer must still accept file 2")
	}

	completeFile(t, stores, id, "f2")
	if !mustTryActivate(t, stores, id) {
		t.Fatal("did not activate after 2 of 2 files")
	}
	if mustValidateForTUS(t, stores, id) {
		t.Fatal("active transfer must not accept new uploads")
	}
}

// A TUS client that restarts an upload after a 404 leaves the old row behind
// as 'uploading'. That row must not block activation.
func TestTryActivate_StrayUploadingRowDoesNotBlock(t *testing.T) {
	stores := newRaceTestStores(t)
	id := newExpectedTransfer(t, stores, 1)
	if err := stores.Transfers.CreateFileRow("stray", id, "a.mov", "p/stray", 1); err != nil {
		t.Fatal(err)
	}
	completeFile(t, stores, id, "retry")
	if !mustTryActivate(t, stores, id) {
		t.Fatal("stray uploading row blocked activation")
	}
}

// Transfers created before migration 004 have no expected_files and keep the
// old rule, so uploads in flight during a deploy still activate.
func TestTryActivate_LegacyTransferKeepsOldRule(t *testing.T) {
	stores := newRaceTestStores(t)
	id := newExpectedTransfer(t, stores, 0)
	if err := stores.Transfers.CreateFileRow("open", id, "a.mov", "p/open", 1); err != nil {
		t.Fatal(err)
	}
	completeFile(t, stores, id, "done")
	if mustTryActivate(t, stores, id) {
		t.Fatal("legacy transfer activated with an incomplete file")
	}
	if err := stores.Transfers.SetFileComplete("open", 1); err != nil {
		t.Fatal(err)
	}
	if !mustTryActivate(t, stores, id) {
		t.Fatal("legacy transfer did not activate once all files were complete")
	}
}

// Abandoned uploads leave a transfer pending; it must still expire, or its
// complete files would never be cleaned up.
func TestGetExpired_IncludesPendingTransfers(t *testing.T) {
	stores := newRaceTestStores(t)
	res, err := stores.Transfers.Create(CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(-time.Minute),
		Recipients: []string{"bob@example.com"}, ExpectedFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := stores.Transfers.GetExpired()
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != res.TransferID {
		t.Fatalf("GetExpired = %+v, want the pending transfer", expired)
	}
}
