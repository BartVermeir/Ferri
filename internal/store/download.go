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

// DownloadedFile is one file of a download, for RecordDownloads.
type DownloadedFile struct {
	ID           string // empty for none: file_id is then NULL
	OriginalName string
}

// RecordDownload inserts a download event and updates the recipient's counters.
// Runs in a single transaction. Returns the inserted event ID, and recent =
// true when the recipient already downloaded something within window before
// this one. A recipient row belongs to one transfer, so the sender gets at
// most one download notification per recipient and transfer per window,
// however many files it holds. Checking inside the same transaction (the pool
// has one connection) means two simultaneous downloads cannot both mail.
func (s *DownloadStore) RecordDownload(recipientID, fileID, originalName, ip, ua string, window time.Duration) (eventID string, recent bool, err error) {
	ids, recent, err := s.RecordDownloads(recipientID, []DownloadedFile{{ID: fileID, OriginalName: originalName}}, ip, ua, window)
	if len(ids) == 1 {
		eventID = ids[0]
	}
	return eventID, recent, err
}

// RecordDownloads is RecordDownload for several files at once (a ZIP), in one
// transaction: recent is decided before any of these events, and every file
// counts once in download_count. One commit instead of one per file, so a ZIP
// of thousands of files does not wait for thousands of fsyncs before its
// first byte, holding the single DB connection all that time.
func (s *DownloadStore) RecordDownloads(recipientID string, files []DownloadedFile, ip, ua string, window time.Duration) (eventIDs []string, recent bool, err error) {
	if len(files) == 0 {
		return nil, false, nil
	}
	eventIDs = make([]string, len(files))
	for i := range files {
		eventIDs[i] = token.Generate()
	}

	err = txFunc(s.db, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM download_events
			WHERE recipient_id = ? AND downloaded_at > unixepoch() - ?`,
			recipientID, int64(window.Seconds()),
		).Scan(&n); err != nil {
			return err
		}
		recent = n > 0

		stmt, err := tx.Prepare(`
			INSERT INTO download_events (id, recipient_id, file_id, original_name, ip_address, user_agent)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, f := range files {
			// file_id is a nullable FK in the schema — convert empty string to
			// nil so the DB receives NULL rather than an empty string foreign key.
			var fileIDVal interface{}
			if f.ID != "" {
				fileIDVal = f.ID
			}
			if _, err := stmt.Exec(
				eventIDs[i], recipientID, fileIDVal, f.OriginalName,
				sql.NullString{String: ip, Valid: ip != ""},
				sql.NullString{String: ua, Valid: ua != ""},
			); err != nil {
				return err
			}
		}

		_, err = tx.Exec(`
			UPDATE recipients
			SET download_count      = download_count + ?,
			    first_download_at   = COALESCE(first_download_at, unixepoch())
			WHERE id = ?`,
			len(files), recipientID,
		)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return eventIDs, recent, nil
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
