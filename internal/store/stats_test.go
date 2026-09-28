package store

// Tests for the admin statistics and for purging deleted items.

import (
	"testing"
	"time"
)

func countRows(t *testing.T, s *Stores, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Transfers.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUploadStats_NetOnlyWhenEveryFileHasIt(t *testing.T) {
	s := newRaceTestStores(t)
	id := newExpectedTransfer(t, s, 2)
	completeFile(t, s, id, "f1")
	completeFile(t, s, id, "f2")
	if err := s.Transfers.UpdateTUSActivity("f1", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	u, err := s.Stats.TransferUploadStats(id)
	if err != nil {
		t.Fatal(err)
	}
	if u.Files != 2 || u.Bytes != 2 || u.NetKnown {
		t.Fatalf("stats = %+v, want 2 files, 2 bytes, net unknown (f2 has none)", u)
	}
	if err := s.Transfers.UpdateTUSActivity("f2", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.Stats.TransferUploadStats(id); !u.NetKnown || u.NetMS != 2000 {
		t.Fatalf("stats = %+v, want net 2000 ms", u)
	}
}

func TestRecordStream_ListedPerItem(t *testing.T) {
	s := newRaceTestStores(t)
	id := newExpectedTransfer(t, s, 1)
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, d := range []DownloadStream{
		{TransferID: id, Who: "bob@example.com", What: "a.mov", Offset: 0, Bytes: 40, Total: 100, StartedAt: start, Duration: 2 * time.Second},
		{TransferID: id, Who: "bob@example.com", What: "a.mov", Offset: 40, Bytes: 60, Total: 100, StartedAt: start.Add(time.Second), Duration: 3 * time.Second},
	} {
		if err := s.Stats.RecordStream(d); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Stats.TransferStreams(id)
	if err != nil || len(list) != 2 {
		t.Fatalf("streams: %v, %d", err, len(list))
	}
	if list[0].Complete() || !list[1].Complete() || list[1].Duration != 3*time.Second || !list[0].StartedAt.Equal(start) {
		t.Fatalf("streams = %+v", list)
	}
}

// A deleted item leaves the database after the retention, with what hangs on
// it; a recent one, one with data possibly left on storage, and a live one stay.
func TestPurgeDeleted(t *testing.T) {
	s := newRaceTestStores(t)
	old := newExpectedTransfer(t, s, 1)
	completeFile(t, s, old, "old-f")
	recent := newExpectedTransfer(t, s, 1)
	leftover := newExpectedTransfer(t, s, 1)
	completeFile(t, s, leftover, "left-f")
	if err := s.Transfers.SetTUSUploadID("left-f", "tus-left"); err != nil {
		t.Fatal(err)
	}
	live := newExpectedTransfer(t, s, 1)
	if err := s.Stats.RecordStream(DownloadStream{TransferID: old, Who: "x", What: "y", Bytes: 1, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{old, recent, leftover} {
		if err := s.Transfers.SoftDelete(id); err != nil {
			t.Fatal(err)
		}
	}
	// A second soft delete (the cleanup job does that) keeps the first time.
	s.Transfers.db.Exec(`UPDATE transfers SET deleted_at = unixepoch() - 20*86400 WHERE id IN (?, ?)`, old, leftover)
	if err := s.Transfers.SoftDelete(old); err != nil {
		t.Fatal(err)
	}

	n, err := s.Transfers.PurgeDeleted(14 * 24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM transfers WHERE id = ?`, old) != 0 {
		t.Fatal("old deleted transfer is still there")
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM files WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM recipients WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM download_streams WHERE transfer_id = ?`,
	} {
		if countRows(t, s, q, old) != 0 {
			t.Errorf("not cascaded: %s", q)
		}
	}
	for _, id := range []string{recent, leftover, live} {
		if countRows(t, s, `SELECT COUNT(*) FROM transfers WHERE id = ?`, id) != 1 {
			t.Errorf("transfer %s was purged, should stay", id)
		}
	}
}

func TestPurgeDeleted_Requests(t *testing.T) {
	s := newRaceTestStores(t)
	id, _, err := s.Requests.Create(CreateRequestInput{Title: "R", RequesterEmail: "a@example.com", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Requests.SoftDelete(id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Requests.PurgeDeleted(14 * 24 * time.Hour); n != 0 {
		t.Fatalf("purged a request deleted just now")
	}
	s.Requests.db.Exec(`UPDATE upload_requests SET deleted_at = unixepoch() - 15*86400 WHERE id = ?`, id)
	if n, err := s.Requests.PurgeDeleted(14 * 24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
}
