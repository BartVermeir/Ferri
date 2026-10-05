package store

// Statistics per transfer and request for the admin: the
// upload from the file rows, the downloads from download_streams. And the
// transfer history: every download stream and upload session of what is
// still live, newest first.

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
	// Net is the time at least one of the complete files was uploading,
	// from their upload sessions: files that upload at the same time count
	// once. NetKnown only when every complete file has a session (files
	// uploaded before upload_sessions existed have none).
	Net      time.Duration
	NetKnown bool
	// IPs the complete files were uploaded from, in the order they first
	// appear; empty for files uploaded before upload_sessions existed.
	IPs []string
}

// TransferUploadStats returns the upload side of a transfer.
func (s *StatsStore) TransferUploadStats(transferID string) (UploadStats, error) {
	return s.uploadStats("files", "transfer_id", "transfer_id", transferID)
}

// RequestUploadStats returns the upload side of an upload request.
func (s *StatsStore) RequestUploadStats(requestID string) (UploadStats, error) {
	return s.uploadStats("upload_request_files", "upload_request_id", "request_id", requestID)
}

// uploadStats: table, column and sessionColumn are one of two fixed sets,
// never input.
func (s *StatsStore) uploadStats(table, column, sessionColumn, id string) (UploadStats, error) {
	var u UploadStats
	var start, end sql.NullInt64
	var withoutSession int
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0), MIN(created_at),
		       MAX(COALESCE(tus_last_activity_at, created_at)),
		       COALESCE(SUM(NOT EXISTS (SELECT 1 FROM upload_sessions s WHERE s.tus_upload_id = f.tus_upload_id)), 0)
		FROM %s f WHERE %s = ? AND status = 'complete'`, table, column), id,
	).Scan(&u.Files, &u.Bytes, &start, &end, &withoutSession)
	if err != nil {
		return u, err
	}
	if start.Valid {
		u.Start = time.Unix(start.Int64, 0)
	}
	if end.Valid {
		u.End = time.Unix(end.Int64, 0)
	}
	u.NetKnown = u.Files > 0 && withoutSession == 0
	if u.Files == 0 {
		return u, nil
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT s.started_at, s.ended_at, COALESCE(s.ip_address, '')
		FROM upload_sessions s
		JOIN %s f ON f.tus_upload_id = s.tus_upload_id AND f.%s = s.%s
		WHERE f.%s = ? AND f.status = 'complete'
		ORDER BY s.started_at`, table, column, sessionColumn, column), id)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	var spans [][2]int64
	seen := map[string]bool{}
	for rows.Next() {
		var sp [2]int64
		var ip string
		if err := rows.Scan(&sp[0], &sp[1], &ip); err != nil {
			return u, err
		}
		spans = append(spans, sp)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			u.IPs = append(u.IPs, ip)
		}
	}
	if err := rows.Err(); err != nil {
		return u, err
	}
	if u.NetKnown {
		u.Net = time.Duration(Covered(spans)) * time.Second
	}
	return u, nil
}

// Covered is how much time the spans (start, end), sorted by start, cover
// together, in the unit of the spans: where they overlap, it counts once.
func Covered(spans [][2]int64) int64 {
	var total, curStart, curEnd int64
	for i, sp := range spans {
		if i == 0 || sp[0] > curEnd {
			total += curEnd - curStart
			curStart, curEnd = sp[0], sp[1]
			continue
		}
		curEnd = max(curEnd, sp[1])
	}
	return total + curEnd - curStart
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

// UploadSessionGap: a chunk that starts within this time after the previous
// chunk of the same file ended, from the same address, continues its
// session. tus-js-client sends the chunks of a file back to back; a longer
// gap is a pause or a broken connection, and shows as a new row.
const UploadSessionGap = 5 * time.Minute

// UploadChunk is one finished TUS PATCH. Exactly one of TransferID and
// RequestID is set.
type UploadChunk struct {
	TransferID  string
	RequestID   string
	TUSUploadID string
	Who         string
	What        string
	IP          string
	Offset      int64 // Upload-Offset
	Bytes       int64
	Total       int64 // size of the file
	StartedAt   time.Time
	Duration    time.Duration
}

// RecordUploadChunk adds a chunk to its file's latest upload session, or
// starts a new session after a pause or from another address. The chunks
// of one file arrive one after the other, never at the same time.
func (s *StatsStore) RecordUploadChunk(c UploadChunk) error {
	end := c.StartedAt.Add(c.Duration)
	res, err := s.db.Exec(`
		UPDATE upload_sessions
		SET bytes_sent = bytes_sent + ?, duration_ms = duration_ms + ?, ended_at = ?
		WHERE id = (SELECT id FROM upload_sessions WHERE tus_upload_id = ? ORDER BY ended_at DESC, rowid DESC LIMIT 1)
		  AND COALESCE(ip_address, '') = ?
		  AND ended_at >= ?`,
		c.Bytes, c.Duration.Milliseconds(), end.Unix(),
		c.TUSUploadID, c.IP, c.StartedAt.Add(-UploadSessionGap).Unix(),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	_, err = s.db.Exec(`
		INSERT INTO upload_sessions
		    (id, transfer_id, request_id, tus_upload_id, who, what, ip_address,
		     offset_bytes, bytes_sent, total_bytes, started_at, ended_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.Generate(), nullIfEmpty(c.TransferID), nullIfEmpty(c.RequestID), c.TUSUploadID, c.Who, c.What, nullIfEmpty(c.IP),
		c.Offset, c.Bytes, c.Total, c.StartedAt.Unix(), end.Unix(), c.Duration.Milliseconds(),
	)
	return err
}

// HistoryEntry is one line of the transfer history: a download stream or
// an upload session. For an upload, Offset is where the session started.
type HistoryEntry struct {
	Upload bool
	Title  string
	End    time.Time // end of the stream or of the session's last chunk
	DownloadStream
}

// History returns every download stream and upload session of transfers
// and requests that are still live (not expired, not deleted), newest
// first. What expired or was deleted is gone from the history.
func (s *StatsStore) History() ([]HistoryEntry, error) {
	const live = `
		LEFT JOIN transfers t ON t.id = x.transfer_id
		LEFT JOIN upload_requests r ON r.id = x.request_id
		WHERE (t.status IN ('pending', 'active') AND t.expires_at > unixepoch())
		   OR (r.status IN ('open', 'completed') AND r.expires_at > unixepoch())`
	const cols = `COALESCE(x.transfer_id, ''), COALESCE(x.request_id, ''), COALESCE(t.title, r.title, ''),
		x.who, x.what, COALESCE(x.ip_address, ''), x.offset_bytes, x.bytes_sent, x.total_bytes, x.started_at AS started, x.duration_ms`
	rows, err := s.db.Query(`
		SELECT 0, ` + cols + `, x.started_at + (x.duration_ms + 999) / 1000 FROM download_streams x` + live + `
		UNION ALL
		SELECT 1, ` + cols + `, x.ended_at FROM upload_sessions x` + live + `
		ORDER BY started DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var started, ms, end int64
		if err := rows.Scan(&e.Upload, &e.TransferID, &e.RequestID, &e.Title, &e.Who, &e.What, &e.IP,
			&e.Offset, &e.Bytes, &e.Total, &started, &ms, &end); err != nil {
			return nil, err
		}
		e.StartedAt = time.Unix(started, 0)
		e.End = time.Unix(end, 0)
		e.Duration = time.Duration(ms) * time.Millisecond
		list = append(list, e)
	}
	return list, rows.Err()
}
