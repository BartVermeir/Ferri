package store

import (
	"database/sql"
	"time"

	"github.com/BartVermeir/Ferri/internal/token"
)

type DownloadStore struct {
	db *sql.DB
}

// DownloadEvent represents a row in download_events.
type DownloadEvent struct {
	ID           string
	RecipientID  string
	FileID       sql.NullString
	OriginalName string
	IPAddress    sql.NullString
	UserAgent    sql.NullString
	DownloadedAt int64
}

// RecordDownload inserts a download event and updates the recipient's counters.
// Runs in a single transaction. Returns the inserted event ID.
func (s *DownloadStore) RecordDownload(recipientID, fileID, originalName, ip, ua string) (string, error) {
	eventID := token.Generate()

	// file_id is a nullable FK in the schema — convert empty string to nil so
	// the DB receives NULL rather than an empty string foreign key.
	var fileIDVal interface{}
	if fileID != "" {
		fileIDVal = fileID
	}

	return eventID, txFunc(s.db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO download_events (id, recipient_id, file_id, original_name, ip_address, user_agent)
			VALUES (?, ?, ?, ?, ?, ?)`,
			eventID, recipientID, fileIDVal, originalName,
			sql.NullString{String: ip, Valid: ip != ""},
			sql.NullString{String: ua, Valid: ua != ""},
		); err != nil {
			return err
		}

		_, err := tx.Exec(`
			UPDATE recipients
			SET download_count      = download_count + 1,
			    first_download_at   = COALESCE(first_download_at, unixepoch())
			WHERE id = ?`,
			recipientID,
		)
		return err
	})
}

// DownloadedWithin reports whether the recipient already has a download event
// for this file within the given window. Used to send at most one download
// notification per recipient and file per window: a download manager or a
// browser retrying a big file must not flood the sender's inbox.
func (s *DownloadStore) DownloadedWithin(recipientID, fileID string, window time.Duration) (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM download_events
		WHERE recipient_id = ? AND file_id = ?
		  AND downloaded_at > unixepoch() - ?`,
		recipientID, fileID, int64(window.Seconds()),
	).Scan(&n)
	return n > 0, err
}

// GetDownloadHistory returns all download events for a transfer,
// grouped by recipient. Used for the expiry summary mail.
// Note: uses download_events.original_name (not files.original_name)
// so filenames are available even after file deletion.
type RecipientHistory struct {
	Email         string
	DownloadCount int
	IsSender      bool // the sender's own link, not a real recipient
	Events        []DownloadEvent
}

func (s *DownloadStore) GetHistoryForTransfer(transferID string) ([]RecipientHistory, error) {
	rows, err := s.db.Query(`
		SELECT r.email, r.download_count, r.is_sender,
		       de.id, de.recipient_id, de.file_id, de.original_name,
		       de.ip_address, de.user_agent, de.downloaded_at
		FROM recipients r
		LEFT JOIN download_events de ON de.recipient_id = r.id
		WHERE r.transfer_id = ?
		ORDER BY r.is_sender, r.email, de.downloaded_at ASC`,
		transferID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RecipientHistory
	emailIdx := make(map[string]int)

	for rows.Next() {
		var (
			email         string
			downloadCount int
			isSender      bool
			ev            DownloadEvent
			eventID       sql.NullString
			recipientID   sql.NullString
			originalName  sql.NullString // nullable: LEFT JOIN produces NULL when no downloads
			downloadedAt  sql.NullInt64
		)
		if err := rows.Scan(
			&email, &downloadCount, &isSender,
			&eventID, &recipientID, &ev.FileID, &originalName,
			&ev.IPAddress, &ev.UserAgent, &downloadedAt,
		); err != nil {
			return nil, err
		}

		idx, exists := emailIdx[email]
		if !exists {
			result = append(result, RecipientHistory{
				Email:         email,
				DownloadCount: downloadCount,
				IsSender:      isSender,
			})
			idx = len(result) - 1
			emailIdx[email] = idx
		}

		if eventID.Valid {
			ev.ID = eventID.String
			ev.RecipientID = recipientID.String
			ev.OriginalName = originalName.String // safe: only used when eventID.Valid
			ev.DownloadedAt = downloadedAt.Int64
			result[idx].Events = append(result[idx].Events, ev)
		}
	}

	return result, rows.Err()
}
