package store

// Tests for the admin statistics and for purging deleted items.

import (
	"strings"
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

// Net upload time comes from the upload sessions of the complete files:
// files that upload at the same time count once, and it is only known when
// every complete file has a session.
func TestUploadStats_NetCountsParallelFilesOnce(t *testing.T) {
	s := newRaceTestStores(t)
	id := newExpectedTransfer(t, s, 3)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	session := func(fileID, ip string, from, to time.Duration) {
		t.Helper()
		if err := s.Stats.RecordUploadChunk(UploadChunk{
			TransferID: id, TUSUploadID: "tus-" + fileID, Who: "alice@example.com", What: fileID, IP: ip,
			Bytes: 1, Total: 1, StartedAt: start.Add(from), Duration: to - from,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"f1", "f2", "f3", "failed"} {
		if err := s.Transfers.CreateFileRow(f, id, f+".mov", "p/"+f, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.Transfers.SetTUSUploadID(f, "tus-"+f); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"f1", "f2", "f3"} {
		if err := s.Transfers.SetFileComplete(f, 1); err != nil {
			t.Fatal(err)
		}
	}
	session("f1", "192.0.2.1", 0, 100*time.Second)
	session("f2", "192.0.2.1", 0, 100*time.Second) // same time as f1: counts once
	session("failed", "192.0.2.9", 200*time.Second, 900*time.Second)

	u, err := s.Stats.TransferUploadStats(id)
	if err != nil {
		t.Fatal(err)
	}
	if u.Files != 3 || u.Bytes != 3 || u.NetKnown {
		t.Fatalf("stats = %+v, want 3 files, 3 bytes, net unknown (f3 has no session)", u)
	}
	session("f3", "192.0.2.2", 50*time.Second, 160*time.Second) // overlaps the first 50 s
	if u, _ = s.Stats.TransferUploadStats(id); !u.NetKnown || u.Net != 160*time.Second {
		t.Fatalf("stats = %+v, want net 160 s (0-160, the failed file left out)", u)
	}
	if got := strings.Join(u.IPs, ","); got != "192.0.2.1,192.0.2.2" {
		t.Fatalf("IPs = %q, want each address of the complete files once, the failed file left out", got)
	}
}

func TestCovered(t *testing.T) {
	for _, tc := range []struct {
		spans [][2]int64
		want  int64
	}{
		{nil, 0},
		{[][2]int64{{10, 20}}, 10},
		{[][2]int64{{0, 10}, {0, 10}, {5, 12}}, 12},
		{[][2]int64{{0, 10}, {20, 25}}, 15},
		{[][2]int64{{0, 30}, {5, 10}, {40, 41}}, 31},
	} {
		if got := Covered(tc.spans); got != tc.want {
			t.Errorf("Covered(%v) = %d, want %d", tc.spans, got, tc.want)
		}
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

// A deleted item leaves the database at once, with what hangs on it; one
// with data possibly left on storage, and a live one, stay.
func TestPurgeDeleted(t *testing.T) {
	s := newRaceTestStores(t)
	gone := newExpectedTransfer(t, s, 1)
	completeFile(t, s, gone, "gone-f")
	leftover := newExpectedTransfer(t, s, 1)
	completeFile(t, s, leftover, "left-f")
	if err := s.Transfers.SetTUSUploadID("left-f", "tus-left"); err != nil {
		t.Fatal(err)
	}
	live := newExpectedTransfer(t, s, 1)
	if err := s.Stats.RecordStream(DownloadStream{TransferID: gone, Who: "x", What: "y", Bytes: 1, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stats.RecordUploadChunk(UploadChunk{TransferID: gone, TUSUploadID: "tus-gone", Who: "x", What: "y", Bytes: 1, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Mail.Enqueue(nil, MailAbout{TransferID: gone}, "bob@example.com", "s", "h", "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mail.Enqueue(nil, MailAbout{}, "admin@example.com", "alert", "h", "t"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{gone, leftover} {
		if err := s.Transfers.SoftDelete(id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Transfers.PurgeDeleted()
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM transfers WHERE id = ?`,
		`SELECT COUNT(*) FROM files WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM recipients WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM download_streams WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM upload_sessions WHERE transfer_id = ?`,
		`SELECT COUNT(*) FROM mail_queue WHERE transfer_id = ?`,
	} {
		if countRows(t, s, q, gone) != 0 {
			t.Errorf("not gone: %s", q)
		}
	}
	if countRows(t, s, `SELECT COUNT(*) FROM mail_queue WHERE transfer_id IS NULL`) != 1 {
		t.Error("a mail about no transfer went too")
	}
	for _, id := range []string{leftover, live} {
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
	if err := s.Mail.Enqueue(nil, MailAbout{RequestID: id}, "a@example.com", "s", "h", "t"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Requests.PurgeDeleted(); n != 0 {
		t.Fatalf("purged an open request")
	}
	if err := s.Requests.SoftDelete(id); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Requests.PurgeDeleted(); err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM mail_queue WHERE request_id = ?`, id) != 0 {
		t.Error("the request's mail is still there")
	}
}

// Chunks of one file that follow each other make one upload session; a
// pause or another address starts a new one.
func TestRecordUploadChunk_MergesUntilPause(t *testing.T) {
	s := newRaceTestStores(t)
	id := newExpectedTransfer(t, s, 1)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	chunk := func(at time.Duration, ip string, offset int64) {
		t.Helper()
		if err := s.Stats.RecordUploadChunk(UploadChunk{
			TransferID: id, TUSUploadID: "tus-a", Who: "alice@example.com", What: "a.mov", IP: ip,
			Offset: offset, Bytes: 100, Total: 400, StartedAt: start.Add(at), Duration: 10 * time.Second,
		}); err != nil {
			t.Fatal(err)
		}
	}
	chunk(0, "192.0.2.1", 0)
	chunk(10*time.Second, "192.0.2.1", 100)                // straight after: same session
	chunk(20*time.Minute, "192.0.2.1", 200)                // after a pause: new session
	chunk(20*time.Minute+10*time.Second, "192.0.2.9", 300) // other address: new session

	list, err := s.Stats.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("sessions = %d, want 3: %+v", len(list), list)
	}
	oldest := list[2]
	if !oldest.Upload || oldest.Bytes != 200 || oldest.Offset != 0 || oldest.Duration != 20*time.Second || !oldest.StartedAt.Equal(start) {
		t.Fatalf("first session = %+v, want 200 bytes from 0 in 20 s", oldest)
	}
	if newest := list[0]; newest.IP != "192.0.2.9" || newest.Offset != 300 || !newest.Complete() {
		t.Fatalf("newest session = %+v, want the complete last part from the other address", newest)
	}
}

// The history holds only live items, downloads and uploads, newest first.
func TestHistory_OnlyLiveNewestFirst(t *testing.T) {
	s := newRaceTestStores(t)
	live := newExpectedTransfer(t, s, 1)
	gone := newExpectedTransfer(t, s, 1)
	if err := s.Transfers.SetExpired(gone); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for _, d := range []DownloadStream{
		{TransferID: live, Who: "bob@example.com", What: "a.mov", Bytes: 10, StartedAt: now.Add(-2 * time.Hour), Duration: time.Second},
		{TransferID: gone, Who: "bob@example.com", What: "old.mov", Bytes: 10, StartedAt: now.Add(-time.Hour), Duration: time.Second},
	} {
		if err := s.Stats.RecordStream(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Stats.RecordUploadChunk(UploadChunk{
		TransferID: live, TUSUploadID: "tus-a", Who: "alice@example.com", What: "a.mov",
		Bytes: 10, Total: 10, StartedAt: now.Add(-3 * time.Hour), Duration: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Stats.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Upload || !list[1].Upload || list[0].What != "a.mov" {
		t.Fatalf("history = %+v, want the download then the upload of the live transfer", list)
	}
}
