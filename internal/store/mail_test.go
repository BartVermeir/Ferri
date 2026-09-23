package store

// Tests for MailStore.MarkFailed's exponential backoff schedule
// (2m -> 8m -> 30m -> 2h, matching the doc comment on MarkFailed) and the
// max_attempts ceiling (default 5, see migrations/001_initial.sql) after
// which a mail stops retrying and is marked permanently 'failed'.

import (
	"testing"

	"github.com/BartVermeir/Ferri/internal/db"
)

func newMailTestStore(t *testing.T) *MailStore {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(d).Mail
}

// fetchOne re-reads a single mail_queue row via FetchPending's sibling
// query path (ListFailed for 'failed' rows, otherwise a raw lookup through
// FetchPending semantics is awkward — read directly via a tiny query here).
func fetchMailItem(t *testing.T, s *MailStore, id string) MailItem {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT id, to_address, subject, body_html, body_text,
		       status, attempts, max_attempts, last_attempt_at,
		       next_attempt_at, error_message, created_at
		FROM mail_queue WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("query mail item: %v", err)
	}
	defer rows.Close()
	items, err := scanMailItems(rows)
	if err != nil {
		t.Fatalf("scan mail item: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly one row for id %s, got %d", id, len(items))
	}
	return items[0]
}

func mustEnqueue(t *testing.T, s *MailStore) string {
	t.Helper()
	if err := s.Enqueue(nil, "bob@example.com", "subject", "<p>hi</p>", "hi"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pending, err := s.FetchPending(1)
	if err != nil {
		t.Fatalf("fetch pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending item, got %d", len(pending))
	}
	return pending[0].ID
}

func TestMarkFailed_BackoffSchedule(t *testing.T) {
	s := newMailTestStore(t)
	id := mustEnqueue(t, s)

	// Backoff minutes per doc comment: 2, 8, 30, 120 for attempts 1-4.
	wantBackoffMinutes := []int64{2, 8, 30, 120}

	for i, wantMinutes := range wantBackoffMinutes {
		if err := s.MarkFailed(id, "smtp error"); err != nil {
			t.Fatalf("attempt %d: mark failed: %v", i+1, err)
		}
		item := fetchMailItem(t, s, id)

		if item.Status != "pending" {
			t.Fatalf("attempt %d: status = %q, want pending (retry scheduled)", i+1, item.Status)
		}
		if item.Attempts != i+1 {
			t.Fatalf("attempt %d: attempts = %d, want %d", i+1, item.Attempts, i+1)
		}
		if !item.LastAttemptAt.Valid {
			t.Fatalf("attempt %d: last_attempt_at not set", i+1)
		}
		// last_attempt_at and next_attempt_at are both computed from
		// unixepoch() within the same UPDATE statement; allow 1s of slack
		// for the (extremely unlikely) case the two calls straddle a
		// second boundary.
		gotDelay := item.NextAttemptAt - item.LastAttemptAt.Int64
		wantDelay := wantMinutes * 60
		if gotDelay < wantDelay-1 || gotDelay > wantDelay+1 {
			t.Fatalf("attempt %d: next_attempt_at - last_attempt_at = %ds, want ~%ds", i+1, gotDelay, wantDelay)
		}
	}
}

func TestMarkFailed_StopsRetryingAfterMaxAttempts(t *testing.T) {
	s := newMailTestStore(t)
	id := mustEnqueue(t, s)

	item := fetchMailItem(t, s, id)
	maxAttempts := item.MaxAttempts
	if maxAttempts <= 0 {
		t.Fatalf("expected a positive max_attempts default, got %d", maxAttempts)
	}

	for i := 0; i < maxAttempts; i++ {
		if err := s.MarkFailed(id, "smtp error"); err != nil {
			t.Fatalf("attempt %d: mark failed: %v", i+1, err)
		}
	}

	item = fetchMailItem(t, s, id)
	if item.Status != "failed" {
		t.Fatalf("status after %d failures = %q, want failed", maxAttempts, item.Status)
	}
	if item.Attempts != maxAttempts {
		t.Fatalf("attempts = %d, want %d", item.Attempts, maxAttempts)
	}

	// A permanently failed item must never be picked up again, however far
	// next_attempt_at is in the past.
	pending, err := s.FetchPending(10)
	if err != nil {
		t.Fatalf("fetch pending: %v", err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatalf("permanently failed item %s was returned by FetchPending", id)
		}
	}

	failed, err := s.ListFailed(10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	found := false
	for _, f := range failed {
		if f.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("item %s not present in ListFailed after exhausting retries", id)
	}
}

func TestMarkSent_IncrementsAttemptsAndClearsPending(t *testing.T) {
	s := newMailTestStore(t)
	id := mustEnqueue(t, s)

	if err := s.MarkSent(id); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	item := fetchMailItem(t, s, id)
	if item.Status != "sent" {
		t.Fatalf("status = %q, want sent", item.Status)
	}
	if item.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", item.Attempts)
	}
}
