package store

import (
	"sync"
	"testing"
	"time"
)

// The sender gets at most one download mail per recipient per hour.
// RecordDownload checks for an earlier download inside its own transaction,
// so of many simultaneous downloads exactly one is not "recent"; after the
// window has passed, the next one is not recent again.
func TestRecordDownload_OneNotRecentPerWindow(t *testing.T) {
	stores := newRaceTestStores(t)
	res, err := stores.Transfers.Create(CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com"}, NotifyRecipients: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	recipientID := res.Recipients[0].RecipientID
	record := func() bool {
		_, recent, err := stores.Downloads.RecordDownload(recipientID, "", "a.mov", "", "", time.Hour)
		if err != nil {
			t.Error(err)
		}
		return recent
	}

	const n = 20
	var mu sync.Mutex
	notRecent := 0
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !record() {
				mu.Lock()
				notRecent++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if notRecent != 1 {
		t.Fatalf("%d simultaneous downloads: %d would mail, want 1", n, notRecent)
	}

	db := stores.Downloads.db
	if _, err := db.Exec(`UPDATE download_events SET downloaded_at = unixepoch() - 7200`); err != nil {
		t.Fatal(err)
	}
	if record() {
		t.Fatal("a download two hours later counts as recent, want a new mail")
	}

	var count int
	if err := db.QueryRow(`SELECT download_count FROM recipients WHERE id = ?`, recipientID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n+1 {
		t.Fatalf("download_count = %d, want %d: every download still counts", count, n+1)
	}
}
