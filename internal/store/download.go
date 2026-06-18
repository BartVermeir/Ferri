package store

import (
	"database/sql"

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

// GetDownloadHistory returns all download events for a transfer,
// grouped by recipient. Used for the expiry summary mail.
// Note: uses download_events.original_name (not files.original_name)
// so filenames are available even after file deletion.
type RecipientHistory struct {
	Email         string
	DownloadCount int
	Events        []DownloadEvent
}

func (s *DownloadStore) GetHistoryForTransfer(transferID string) ([]RecipientHistory, error) {
	rows, err := s.db.Query(`
		SELECT r.email, r.download_count,
		       de.id, de.recipient_id, de.file_id, de.original_name,
		       de.ip_address, de.user_agent, de.downloaded_at
		FROM recipients r
		LEFT JOIN download_events de ON de.recipient_id = r.id
		WHERE r.transfer_id = ?
		ORDER BY r.email, de.downloaded_at ASC`,
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
			ev            DownloadEvent
			eventID       sql.NullString
			recipientID   sql.NullString
			originalName  sql.NullString // nullable: LEFT JOIN produces NULL when no downloads
			downloadedAt  sql.NullInt64
		)
		if err := rows.Scan(
			&email, &downloadCount,
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
