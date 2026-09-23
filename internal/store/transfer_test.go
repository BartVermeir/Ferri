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
