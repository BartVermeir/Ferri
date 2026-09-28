package store

// Statistics per transfer and request for the admin: the
// upload from the file rows, the downloads from download_streams.

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/BartVermeir/Ferri/internal/token"
)

type StatsStore struct {
	db *sql.DB
}

// DownloadStream is one finished download: a file, a resumed part of one,
// or a ZIP. Exactly one of TransferID and RequestID is set.
type DownloadStream struct {
	TransferID string
	RequestID  string
	Who        string
	What       string
	IP         string
	Offset     int64 // Range start
	Bytes      int64 // sent on this stream
	Total      int64 // size of the file or of the files in the ZIP; 0 = unknown
	StartedAt  time.Time
	Duration   time.Duration
}

// Complete reports whether the stream reached the end of the file or ZIP.
// A ZIP is a little larger than its files, so reaching Total is enough.
func (d DownloadStream) Complete() bool {
	return d.Total > 0 && d.Offset+d.Bytes >= d.Total
}

// RecordStream stores a finished download stream.
func (s *StatsStore) RecordStream(d DownloadStream) error {
	_, err := s.db.Exec(`
		INSERT INTO download_streams
		    (id, transfer_id, request_id, who, what, ip_address,
		     offset_bytes, bytes_sent, total_bytes, started_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.Generate(), nullIfEmpty(d.TransferID), nullIfEmpty(d.RequestID), d.Who, d.What, nullIfEmpty(d.IP),
		d.Offset, d.Bytes, d.Total, d.StartedAt.Unix(), d.Duration.Milliseconds(),
	)
	return err
}

func nullIfEmpty(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}

// UploadStats is the upload side of an item, from its complete files.
type UploadStats struct {
	Files int   // complete files
	Bytes int64 // their size
	// First file started, last chunk arrived. Zero without complete files.
	Start, End time.Time
	// NetMS is the summed PATCH time; NetKnown only when every complete
	// file has one (files from before migration 009 have 0).
	NetMS    int64
	NetKnown bool
}

// TransferUploadStats returns the upload side of a transfer.
func (s *StatsStore) TransferUploadStats(transferID string) (UploadStats, error) {
	return s.uploadStats("files", "transfer_id", transferID)
}

// RequestUploadStats returns the upload side of an upload request.
func (s *StatsStore) RequestUploadStats(requestID string) (UploadStats, error) {
	return s.uploadStats("upload_request_files", "upload_request_id", requestID)
}

// uploadStats: table and column are one of two fixed pairs, never input.
func (s *StatsStore) uploadStats(table, column, id string) (UploadStats, error) {
	var u UploadStats
	var start, end sql.NullInt64
	var withoutNet int
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0), MIN(created_at),
		       MAX(COALESCE(tus_last_activity_at, created_at)),
		       COALESCE(SUM(upload_ms), 0), COALESCE(SUM(upload_ms = 0), 0)
		FROM %s WHERE %s = ? AND status = 'complete'`, table, column), id,
	).Scan(&u.Files, &u.Bytes, &start, &end, &u.NetMS, &withoutNet)
	if err != nil {
		return u, err
	}
	if start.Valid {
		u.Start = time.Unix(start.Int64, 0)
	}
	if end.Valid {
		u.End = time.Unix(end.Int64, 0)
	}
	u.NetKnown = u.Files > 0 && withoutNet == 0
	return u, nil
}

// TransferStreams returns a transfer's download streams, oldest first.
func (s *StatsStore) TransferStreams(transferID string) ([]DownloadStream, error) {
	return s.streams("transfer_id", transferID)
}

// RequestStreams returns a request's download streams, oldest first.
func (s *StatsStore) RequestStreams(requestID string) ([]DownloadStream, error) {
	return s.streams("request_id", requestID)
}

// streams: column is one of two fixed names, never input.
func (s *StatsStore) streams(column, id string) ([]DownloadStream, error) {
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT COALESCE(transfer_id, ''), COALESCE(request_id, ''), who, what, COALESCE(ip_address, ''),
		       offset_bytes, bytes_sent, total_bytes, started_at, duration_ms
		FROM download_streams WHERE %s = ?
		ORDER BY started_at, rowid`, column), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []DownloadStream
	for rows.Next() {
		var d DownloadStream
		var started, ms int64
		if err := rows.Scan(&d.TransferID, &d.RequestID, &d.Who, &d.What, &d.IP,
			&d.Offset, &d.Bytes, &d.Total, &started, &ms); err != nil {
			return nil, err
		}
		d.StartedAt = time.Unix(started, 0)
		d.Duration = time.Duration(ms) * time.Millisecond
		list = append(list, d)
	}
	return list, rows.Err()
}
