package store

import (
	"database/sql"
	"time"

	"github.com/your-org/ferri/internal/token"
)

type MailStore struct {
	db *sql.DB
}

// MailItem represents a row in the mail_queue table.
type MailItem struct {
	ID            string
	ToAddress     string
	Subject       string
	BodyHTML      string
	BodyText      string
	Status        string
	Attempts      int
	MaxAttempts   int
	LastAttemptAt sql.NullInt64
	NextAttemptAt int64
	ErrorMessage  sql.NullString
	CreatedAt     time.Time
}

// Enqueue inserts a new mail into the queue with status='pending'.
// If tx is non-nil, the insert runs within that transaction.
func (s *MailStore) Enqueue(tx *sql.Tx, to, subject, bodyHTML, bodyText string) error {
	id := token.Generate()
	query := `INSERT INTO mail_queue (id, to_address, subject, body_html, body_text)
	          VALUES (?, ?, ?, ?, ?)`
	args := []any{id, to, subject, bodyHTML, bodyText}

	var err error
	if tx != nil {
		_, err = tx.Exec(query, args...)
	} else {
		_, err = s.db.Exec(query, args...)
	}
	return err
}

// FetchPending atomically sets up to n pending mails to 'sending' and returns them.
// SQLite's single-writer model serialises this — no two goroutines can run it
// concurrently. The SELECT after the UPDATE fetches all 'sending' rows, which may
// include rows stuck from a previous crashed run (recovered by StartupHooks).
func (s *MailStore) FetchPending(n int) ([]MailItem, error) {
	_, err := s.db.Exec(`
		UPDATE mail_queue SET status = 'sending'
		WHERE id IN (
			SELECT id FROM mail_queue
			WHERE status = 'pending' AND next_attempt_at <= unixepoch()
			ORDER BY created_at ASC
			LIMIT ?
		)`, n,
	)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT id, to_address, subject, body_html, body_text,
		       status, attempts, max_attempts, last_attempt_at,
		       next_attempt_at, error_message, created_at
		FROM mail_queue
		WHERE status = 'sending'
		ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMailItems(rows)
}

// MarkSent marks a mail as successfully sent.
func (s *MailStore) MarkSent(id string) error {
	_, err := s.db.Exec(`
		UPDATE mail_queue
		SET status = 'sent', last_attempt_at = unixepoch(), attempts = attempts + 1
		WHERE id = ?`, id,
	)
	return err
}

// MarkFailed records a failed send attempt with exponential backoff.
// Backoff schedule: 2m → 8m → 30m → 2h → permanently failed.
func (s *MailStore) MarkFailed(id string, errMsg string) error {
	var attempts, maxAttempts int
	if err := s.db.QueryRow(
		`SELECT attempts, max_attempts FROM mail_queue WHERE id = ?`, id,
	).Scan(&attempts, &maxAttempts); err != nil {
		return err
	}

	newAttempts := attempts + 1
	if newAttempts >= maxAttempts {
		_, err := s.db.Exec(`
			UPDATE mail_queue
			SET status = 'failed',
			    attempts = ?,
			    last_attempt_at = unixepoch(),
			    error_message = ?
			WHERE id = ?`,
			newAttempts, errMsg, id,
		)
		return err
	}

	// Exponential backoff in minutes: 2, 8, 30, 120
	backoffMinutes := []int{2, 8, 30, 120}
	idx := newAttempts - 1
	if idx >= len(backoffMinutes) {
		idx = len(backoffMinutes) - 1
	}
	backoff := backoffMinutes[idx]

	_, err := s.db.Exec(`
		UPDATE mail_queue
		SET status = 'pending',
		    attempts = ?,
		    last_attempt_at = unixepoch(),
		    next_attempt_at = unixepoch() + ?,
		    error_message = ?
		WHERE id = ?`,
		newAttempts, backoff*60, errMsg, id,
	)
	return err
}

// PruneSent deletes sent mails older than retentionDays.
func (s *MailStore) PruneSent(retentionDays int) (int64, error) {
	result, err := s.db.Exec(`
		DELETE FROM mail_queue
		WHERE status = 'sent'
		  AND last_attempt_at < (unixepoch() - ? * 86400)`,
		retentionDays,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CountFailed returns the number of permanently failed mails.
func (s *MailStore) CountFailed() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mail_queue WHERE status = 'failed'`).Scan(&count)
	return count, err
}

// ListFailed returns failed mails for the admin UI.
func (s *MailStore) ListFailed(limit int) ([]MailItem, error) {
	rows, err := s.db.Query(`
		SELECT id, to_address, subject, body_html, body_text,
		       status, attempts, max_attempts, last_attempt_at,
		       next_attempt_at, error_message, created_at
		FROM mail_queue
		WHERE status = 'failed'
		ORDER BY created_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMailItems(rows)
}

// Retry resets a failed mail to pending with attempts = 0.
func (s *MailStore) Retry(id string) error {
	_, err := s.db.Exec(`
		UPDATE mail_queue
		SET status = 'pending', attempts = 0,
		    next_attempt_at = unixepoch(), error_message = NULL
		WHERE id = ?`, id,
	)
	return err
}

// Delete removes a mail from the queue entirely.
func (s *MailStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM mail_queue WHERE id = ?`, id)
	return err
}

// scanMailItems scans rows from mail_queue.
// created_at is INTEGER (Unix epoch) — scan into int64, convert to time.Time.
func scanMailItems(rows *sql.Rows) ([]MailItem, error) {
	var list []MailItem
	for rows.Next() {
		var m MailItem
		var createdAt int64
		if err := rows.Scan(
			&m.ID, &m.ToAddress, &m.Subject, &m.BodyHTML, &m.BodyText,
			&m.Status, &m.Attempts, &m.MaxAttempts, &m.LastAttemptAt,
			&m.NextAttemptAt, &m.ErrorMessage, &createdAt,
		); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, m)
	}
	return list, rows.Err()
}
