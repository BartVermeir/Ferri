package jobs

// Tests for the requester reminder (reminder.go) and the admin alerts
// (alerts.go), against a real in-memory DB and local storage.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/db"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

func newJobTestEnv(t *testing.T, mgr *storage.Manager) (*Scheduler, *store.Stores, *sql.DB) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	stores := store.New(d)
	if mgr == nil {
		mgr = storage.NewManager(storage.NewLocalBackend(t.TempDir()))
	}
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg := newCleanupTestConfig()
	cfg.Limits.MinFreeBytes = 1 // the test machine's own free space must not raise alerts
	return NewScheduler(cfg, stores, mgr), stores, d
}

// queued returns the subjects of queued mails whose subject starts with prefix.
func queued(t *testing.T, d *sql.DB, prefix string) []string {
	t.Helper()
	rows, err := d.Query(`SELECT subject FROM mail_queue WHERE substr(subject, 1, length(?)) = ? ORDER BY created_at`, prefix, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func exec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// ── Reminder ──────────────────────────────────────────────────────────────────

func TestRequestReminder_OnlyDueRequestsOnce(t *testing.T) {
	s, stores, d := newJobTestEnv(t, nil)
	mk := func(title string, expiresIn, age time.Duration) (string, string) {
		t.Helper()
		id, tok, err := stores.Requests.Create(store.CreateRequestInput{
			Title: title, RequesterName: "Alice", RequesterEmail: "alice@example.com",
			ExpiresAt: time.Now().Add(expiresIn),
		})
		if err != nil {
			t.Fatal(err)
		}
		exec(t, d, `UPDATE upload_requests SET created_at = ? WHERE id = ?`, time.Now().Add(-age).Unix(), id)
		return id, tok
	}
	_, dueTok := mk("due", 12*time.Hour, 6*24*time.Hour)
	mk("young", 12*time.Hour, 12*time.Hour)     // a one-day request: never reminded
	mk("later", 3*24*time.Hour, 4*24*time.Hour) // not in its last day yet
	withFile, _ := mk("has a file", 12*time.Hour, 6*24*time.Hour)
	if err := stores.Requests.CreateFileRow("f1", withFile, "a.mov", "p", 1); err != nil {
		t.Fatal(err)
	}
	if err := stores.Requests.SetFileComplete("f1", 1); err != nil {
		t.Fatal(err)
	}

	s.sendRequestReminders()
	s.sendRequestReminders() // a second run must not remind again

	got := queued(t, d, "Nothing uploaded yet")
	if len(got) != 1 || got[0] != `Nothing uploaded yet: "due"` {
		t.Fatalf("reminders = %q, want exactly one for \"due\"", got)
	}
	req, _ := stores.Requests.GetByUploadToken(dueTok)
	items, _ := stores.Mail.FetchPending(10)
	m := items[0]
	if m.ToAddress != "alice@example.com" {
		t.Errorf("reminder went to %s, want the requester", m.ToAddress)
	}
	for _, body := range []string{m.BodyHTML, m.BodyText} {
		for _, want := range []string{"/ul/" + dueTok, "/manage/" + req.ManageToken.String, "Nobody has uploaded files"} {
			if !strings.Contains(body, want) {
				t.Errorf("reminder misses %q", want)
			}
		}
		if strings.Contains(body, req.ViewPathToken()) {
			t.Error("reminder exposes the view token")
		}
	}
}

// ── Alerts ────────────────────────────────────────────────────────────────────

func TestAlerts_NothingWithoutRecipients(t *testing.T) {
	s, _, d := newJobTestEnv(t, nil)
	s.cfg.Limits.MinFreeBytes = 1 << 61 // any disk is "almost full"
	s.runAlertJob()
	if got := queued(t, d, alertSubjectPrefix); len(got) != 0 {
		t.Fatalf("alerts without recipients: %q", got)
	}
}

func TestAlerts_LowSpaceOncePerDay(t *testing.T) {
	s, stores, d := newJobTestEnv(t, nil)
	if err := stores.Settings.Save("alerts.recipients", "it@example.com, ops@example.com"); err != nil {
		t.Fatal(err)
	}
	s.cfg.Limits.MinFreeBytes = 1 << 61

	s.runAlertJob()
	s.runAlertJob()
	got := queued(t, d, alertSubjectPrefix)
	if len(got) != 2 || !strings.HasSuffix(got[0], "storage almost full") {
		t.Fatalf("alerts = %q, want one low-space alert to each of 2 addresses", got)
	}

	exec(t, d, `UPDATE alert_state SET last_sent_at = ? WHERE kind = ?`, time.Now().Add(-25*time.Hour).Unix(), alertLowSpace)
	s.runAlertJob()
	if got := queued(t, d, alertSubjectPrefix); len(got) != 4 {
		t.Fatalf("after 24 h: %d alerts, want 4", len(got))
	}

	s.cfg.Limits.MinFreeBytes = 1 // plenty free now
	s.runAlertJob()
	if st, _ := stores.Alerts.Get(alertLowSpace); !st.Since.IsZero() {
		t.Error("low space not cleared once there is room again")
	}
}

func TestAlerts_StorageDownAfter30Minutes(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A root below a regular file can never be created: storage is "down".
	mgr := storage.NewManager(storage.NewLocalBackend(filepath.Join(blocker, "storage")))
	s, stores, d := newJobTestEnv(t, mgr)
	if err := stores.Settings.Save("alerts.recipients", "it@example.com"); err != nil {
		t.Fatal(err)
	}

	s.runAlertJob()
	if got := queued(t, d, alertSubjectPrefix); len(got) != 0 {
		t.Fatalf("alert on the first failed check: %q", got)
	}
	exec(t, d, `UPDATE alert_state SET since = ? WHERE kind = ?`, time.Now().Add(-31*time.Minute).Unix(), alertStorageDown)
	s.runAlertJob()
	got := queued(t, d, alertSubjectPrefix)
	if len(got) != 1 || !strings.HasSuffix(got[0], "storage not reachable") {
		t.Fatalf("alerts = %q, want one storage-down alert", got)
	}
}

func TestAlerts_LeftoversAfterADay(t *testing.T) {
	s, stores, d := newJobTestEnv(t, nil)
	if err := stores.Settings.Save("alerts.recipients", "it@example.com"); err != nil {
		t.Fatal(err)
	}
	res, err := stores.Transfers.Create(store.CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(time.Hour),
		Recipients: []string{"bob@example.com"},
		Files:      []store.CreateFileInput{{OriginalName: "a.mov", StoragePath: "p", SizeBytes: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID := res.Files[0].FileID
	if err := stores.Transfers.SetTUSUploadID(fileID, "stuck"); err != nil {
		t.Fatal(err)
	}
	if err := stores.Transfers.MarkFileDeleted(fileID); err != nil {
		t.Fatal(err)
	}

	s.runAlertJob()
	if got := queued(t, d, alertSubjectPrefix); len(got) != 0 {
		t.Fatalf("leftover alert right away: %q", got)
	}
	exec(t, d, `UPDATE alert_state SET since = ? WHERE kind = ?`, time.Now().Add(-25*time.Hour).Unix(), alertLeftovers)
	s.runAlertJob()
	if got := queued(t, d, alertSubjectPrefix); len(got) != 1 || !strings.HasSuffix(got[0], "deleted files still on storage") {
		t.Fatalf("alerts = %q, want one leftover alert", got)
	}

	if err := stores.Files.MarkPurged(store.TransferFiles, fileID); err != nil {
		t.Fatal(err)
	}
	s.runAlertJob()
	if st, _ := stores.Alerts.Get(alertLeftovers); !st.Since.IsZero() {
		t.Error("leftovers not cleared once the file is gone")
	}
}

func TestAlerts_FailedMailsReportedOnceSkippingAlerts(t *testing.T) {
	s, stores, d := newJobTestEnv(t, nil)
	if err := stores.Settings.Save("alerts.recipients", "it@example.com"); err != nil {
		t.Fatal(err)
	}
	fail := func(subject string) {
		t.Helper()
		if err := stores.Mail.Enqueue(nil, "nobody@example.org", subject, "<p>x</p>", "x"); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := d.QueryRow(`SELECT id FROM mail_queue WHERE subject = ?`, subject).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if err := stores.Mail.MarkFailed(id, "550 no such user"); err != nil {
				t.Fatal(err)
			}
		}
	}
	fail("Sent: report")
	fail(alertSubjectPrefix + "an older alert")

	s.runAlertJob()
	items, _ := stores.Mail.FetchPending(10)
	if len(items) != 1 || !strings.HasSuffix(items[0].Subject, "1 mail could not be sent") {
		t.Fatalf("want one failed-mail alert, got %d mails", len(items))
	}
	if !strings.Contains(items[0].BodyText, "nobody@example.org") || !strings.Contains(items[0].BodyText, "550 no such user") ||
		strings.Contains(items[0].BodyText, "an older alert") {
		t.Errorf("alert body wrong:\n%s", items[0].BodyText)
	}

	// Next day: the same failure is not reported again, a new one is. The
	// failure is moved before the alert: in the test both fell in the same
	// second, which FailedSince reports twice on purpose.
	exec(t, d, `UPDATE alert_state SET last_sent_at = last_sent_at - 90000 WHERE kind = ?`, alertMailFailed)
	exec(t, d, `UPDATE mail_queue SET last_attempt_at = last_attempt_at - 90060 WHERE subject = 'Sent: report'`)
	failedAlerts := func() int { return len(queued(t, d, alertSubjectPrefix+"My Organisation: ")) }
	s.runAlertJob()
	if n := failedAlerts(); n != 1 {
		t.Fatalf("old failure reported again: %d alerts", n)
	}
	fail("Sent: second")
	s.runAlertJob()
	if n := failedAlerts(); n != 2 {
		t.Fatalf("new failure not reported: %d alerts", n)
	}
}
