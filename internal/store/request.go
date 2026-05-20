package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/your-org/ferri/internal/token"
)

type RequestStore struct {
	db *sql.DB
}

// UploadRequest represents a row in upload_requests.
type UploadRequest struct {
	ID             string
	Title          string
	Message        string
	RequesterName  string
	RequesterEmail string
	UploadToken    string
	PasswordHash   sql.NullString
	MaxFiles       sql.NullInt64
	MaxTotalBytes  sql.NullInt64
	Status         string
	ExpiresAt      time.Time
	CompletedAt    sql.NullInt64
	ExpiredAt      sql.NullInt64
	CreatedAt      time.Time
}

// UploadRequestFile represents a row in upload_request_files.
type UploadRequestFile struct {
	ID               string
	UploadRequestID  string
	OriginalName     string
	StoragePath      string
	SizeBytes        int64
	MimeType         sql.NullString
	TUSUploadID      sql.NullString
	TUSLastActivity  sql.NullInt64
	Status           string
	CreatedAt        time.Time
}

// CreateRequestInput holds all data to create an upload request.
type CreateRequestInput struct {
	Title          string
	Message        string
	RequesterName  string
	RequesterEmail string
	PasswordHash   string
	ExpiresAt      time.Time
	MaxFiles       *int
	MaxTotalBytes  *int64
}

// Create creates a new upload request and returns its upload token.
func (s *RequestStore) Create(input CreateRequestInput) (string, string, error) {
	id := token.Generate()
	uploadToken := token.Generate()

	var passwordHash sql.NullString
	if input.PasswordHash != "" {
		passwordHash = sql.NullString{String: input.PasswordHash, Valid: true}
	}

	var maxFiles sql.NullInt64
	if input.MaxFiles != nil {
		maxFiles = sql.NullInt64{Int64: int64(*input.MaxFiles), Valid: true}
	}

	var maxBytes sql.NullInt64
	if input.MaxTotalBytes != nil {
		maxBytes = sql.NullInt64{Int64: *input.MaxTotalBytes, Valid: true}
	}

	_, err := s.db.Exec(`
		INSERT INTO upload_requests
		  (id, title, message, requester_name, requester_email,
		   upload_token, password_hash, max_files, max_total_bytes,
		   status, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)`,
		id, input.Title, input.Message, input.RequesterName, input.RequesterEmail,
		uploadToken, passwordHash, maxFiles, maxBytes, input.ExpiresAt.Unix(),
	)
	if err != nil {
		return "", "", fmt.Errorf("insert upload_request: %w", err)
	}

	return id, uploadToken, nil
}

// GetByUploadToken looks up an upload request by its token.
// Returns nil if not found, expired, or not open.
func (s *RequestStore) GetByUploadToken(tok string) (*UploadRequest, error) {
	var r UploadRequest
	var expiresAt, createdAt int64
	err := s.db.QueryRow(`
		SELECT id, title, message, requester_name, requester_email,
		       upload_token, password_hash, max_files, max_total_bytes,
		       status, expires_at, completed_at, expired_at, created_at
		FROM upload_requests
		WHERE upload_token = ?
		  AND status = 'open'
		  AND expires_at > unixepoch()`,
		tok,
	).Scan(
		&r.ID, &r.Title, &r.Message, &r.RequesterName, &r.RequesterEmail,
		&r.UploadToken, &r.PasswordHash, &r.MaxFiles, &r.MaxTotalBytes,
		&r.Status, &expiresAt, &r.CompletedAt, &r.ExpiredAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ExpiresAt = time.Unix(expiresAt, 0)
	r.CreatedAt = time.Unix(createdAt, 0)
	return &r, nil
}


// GetByUploadTokenAny looks up an upload request by token regardless of status.
// Used for download pages where completed requests should still be accessible.
func (s *RequestStore) GetByUploadTokenAny(tok string) (*UploadRequest, error) {
	var r UploadRequest
	var expiresAt, createdAt int64
	err := s.db.QueryRow(`
		SELECT id, title, message, requester_name, requester_email,
		       upload_token, password_hash, max_files, max_total_bytes,
		       status, expires_at, completed_at, expired_at, created_at
		FROM upload_requests
		WHERE upload_token = ?`,
		tok,
	).Scan(
		&r.ID, &r.Title, &r.Message, &r.RequesterName, &r.RequesterEmail,
		&r.UploadToken, &r.PasswordHash, &r.MaxFiles, &r.MaxTotalBytes,
		&r.Status, &expiresAt, &r.CompletedAt, &r.ExpiredAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ExpiresAt = time.Unix(expiresAt, 0)
	r.CreatedAt = time.Unix(createdAt, 0)
	return &r, nil
}

// Complete marks an upload request as completed.
func (s *RequestStore) Complete(requestID string) error {
	_, err := s.db.Exec(`
		UPDATE upload_requests
		SET status = 'completed', completed_at = unixepoch()
		WHERE id = ?`, requestID,
	)
	return err
}

// SetExpired marks an upload request as expired.
func (s *RequestStore) SetExpired(requestID string) error {
	_, err := s.db.Exec(`
		UPDATE upload_requests
		SET status = 'expired', expired_at = unixepoch()
		WHERE id = ?`, requestID,
	)
	return err
}

// GetExpired returns open requests past their expiry.
func (s *RequestStore) GetExpired() ([]UploadRequest, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, requester_name, requester_email,
		       upload_token, password_hash, max_files, max_total_bytes,
		       status, expires_at, completed_at, expired_at, created_at
		FROM upload_requests
		WHERE status = 'open' AND expires_at < unixepoch()`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// GetForCleanup returns expired requests past the grace period.
func (s *RequestStore) GetForCleanup(graceHours int) ([]UploadRequest, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, requester_name, requester_email,
		       upload_token, password_hash, max_files, max_total_bytes,
		       status, expires_at, completed_at, expired_at, created_at
		FROM upload_requests
		WHERE status IN ('expired', 'completed')
		  AND expires_at < (unixepoch() - ? * 3600)`,
		graceHours,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// ValidateForTUS checks that an upload request token is valid for TUS upload.
func (s *RequestStore) ValidateForTUS(uploadToken string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(`
		SELECT id FROM upload_requests
		WHERE upload_token = ? AND status = 'open' AND expires_at > unixepoch()`,
		uploadToken,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, true, err
}

// GetFiles returns files for an upload request.
func (s *RequestStore) GetFiles(requestID string) ([]UploadRequestFile, error) {
	rows, err := s.db.Query(`
		SELECT id, upload_request_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM upload_request_files
		WHERE upload_request_id = ? AND status = 'complete'`,
		requestID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequestFiles(rows)
}

// GetStalled returns stalled upload_request_files.
func (s *RequestStore) GetStalled(stallHours int) ([]UploadRequestFile, error) {
	rows, err := s.db.Query(`
		SELECT id, upload_request_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM upload_request_files
		WHERE status = 'uploading'
		  AND COALESCE(tus_last_activity_at, created_at) < (unixepoch() - ? * 3600)`,
		stallHours,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequestFiles(rows)
}

// MarkFileDeleted marks a single upload_request_file as deleted.
func (s *RequestStore) MarkFileDeleted(fileID string) error {
	_, err := s.db.Exec(`UPDATE upload_request_files SET status = 'deleted' WHERE id = ?`, fileID)
	return err
}

// TryComplete atomically sets upload_request to 'completed' if all its files are complete.
func (s *RequestStore) TryComplete(requestID string) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE upload_requests
		SET status = 'completed', completed_at = unixepoch()
		WHERE id = ?
		  AND status = 'open'
		  AND (SELECT COUNT(*) FROM upload_request_files
		       WHERE upload_request_id = ? AND status != 'complete') = 0`,
		requestID, requestID,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}


// CreateFileRow inserts a new upload_request_files row from the TUS callback.

// GetRequestFileByID returns a single upload request file by its ID, reading fresh from DB.
func (s *RequestStore) GetRequestFileByID(fileID string) (*UploadRequestFile, error) {
	var f UploadRequestFile
	var createdAt int64
	err := s.db.QueryRow(`
		SELECT id, upload_request_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM upload_request_files WHERE id = ?`, fileID,
	).Scan(
		&f.ID, &f.UploadRequestID, &f.OriginalName, &f.StoragePath, &f.SizeBytes,
		&f.MimeType, &f.TUSUploadID, &f.TUSLastActivity, &f.Status, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.CreatedAt = time.Unix(createdAt, 0)
	return &f, nil
}

func (s *RequestStore) CreateFileRow(fileID, requestID, originalName, storagePath string, sizeBytes int64) error {
	_, err := s.db.Exec(`
		INSERT INTO upload_request_files
		  (id, upload_request_id, original_name, storage_path, size_bytes, status)
		VALUES (?, ?, ?, ?, ?, 'uploading')`,
		fileID, requestID, originalName, storagePath, sizeBytes,
	)
	return err
}

// SetTUSUploadID records the tusd-generated upload ID on the request file row.
func (s *RequestStore) SetTUSUploadID(fileID, tusUploadID string) error {
	_, err := s.db.Exec(
		`UPDATE upload_request_files SET tus_upload_id = ? WHERE id = ?`, tusUploadID, fileID,
	)
	return err
}

// GetFileIDByTUSID looks up the Ferri file ID by the tusd upload ID for request files.
// Returns empty string if not found.
func (s *RequestStore) GetFileIDByTUSID(tusUploadID string) (string, error) {
	var fileID string
	err := s.db.QueryRow(
		`SELECT id FROM upload_request_files WHERE tus_upload_id = ?`, tusUploadID,
	).Scan(&fileID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return fileID, err
}

// SetFileComplete marks an upload_request_files row as complete with final size.
func (s *RequestStore) SetFileComplete(fileID string, sizeBytes int64) error {
	_, err := s.db.Exec(`
		UPDATE upload_request_files
		SET status = 'complete', size_bytes = ?, tus_upload_id = NULL
		WHERE id = ?`,
		sizeBytes, fileID,
	)
	return err
}

// UpdateTUSActivity updates tus_last_activity_at for a request file row.
func (s *RequestStore) UpdateTUSActivity(fileID string) error {
	_, err := s.db.Exec(
		`UPDATE upload_request_files SET tus_last_activity_at = unixepoch() WHERE id = ?`, fileID,
	)
	return err
}

// scanRequests scans rows from upload_requests.
// expires_at and created_at are INTEGER (Unix epoch) — scan into int64, convert to time.Time.
func scanRequests(rows *sql.Rows) ([]UploadRequest, error) {
	var list []UploadRequest
	for rows.Next() {
		var r UploadRequest
		var expiresAt, createdAt int64
		if err := rows.Scan(
			&r.ID, &r.Title, &r.Message, &r.RequesterName, &r.RequesterEmail,
			&r.UploadToken, &r.PasswordHash, &r.MaxFiles, &r.MaxTotalBytes,
			&r.Status, &expiresAt, &r.CompletedAt, &r.ExpiredAt, &createdAt,
		); err != nil {
			return nil, err
		}
		r.ExpiresAt = time.Unix(expiresAt, 0)
		r.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, r)
	}
	return list, rows.Err()
}

// scanRequestFiles scans rows from upload_request_files.
// created_at is INTEGER (Unix epoch) — scan into int64, convert to time.Time.
func scanRequestFiles(rows *sql.Rows) ([]UploadRequestFile, error) {
	var list []UploadRequestFile
	for rows.Next() {
		var f UploadRequestFile
		var createdAt int64
		if err := rows.Scan(
			&f.ID, &f.UploadRequestID, &f.OriginalName, &f.StoragePath, &f.SizeBytes,
			&f.MimeType, &f.TUSUploadID, &f.TUSLastActivity, &f.Status, &createdAt,
		); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, f)
	}
	return list, rows.Err()
}

// RequestSummary enriches UploadRequest with file stats for the admin overview.
type RequestSummary struct {
	UploadRequest
	FileCount  int
	TotalBytes int64
}

// ListForAdmin returns all non-expired, non-deleted upload requests with file counts.
func (s *RequestStore) ListForAdmin(limit int) ([]RequestSummary, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.title, r.message, r.requester_name, r.requester_email,
		       r.upload_token, r.password_hash, r.max_files, r.max_total_bytes,
		       r.status, r.expires_at, r.completed_at, r.expired_at, r.created_at,
		       COUNT(f.id)                    AS file_count,
		       COALESCE(SUM(f.size_bytes), 0) AS total_bytes
		FROM upload_requests r
		LEFT JOIN upload_request_files f ON f.upload_request_id = r.id AND f.status = 'complete'
		WHERE r.status NOT IN ('expired', 'deleted')
		GROUP BY r.id
		ORDER BY r.created_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []RequestSummary
	for rows.Next() {
		var rs RequestSummary
		var expiresAt, createdAt int64
		if err := rows.Scan(
			&rs.ID, &rs.Title, &rs.Message, &rs.RequesterName, &rs.RequesterEmail,
			&rs.UploadToken, &rs.PasswordHash, &rs.MaxFiles, &rs.MaxTotalBytes,
			&rs.Status, &expiresAt, &rs.CompletedAt, &rs.ExpiredAt, &createdAt,
			&rs.FileCount, &rs.TotalBytes,
		); err != nil {
			return nil, err
		}
		rs.ExpiresAt = time.Unix(expiresAt, 0)
		rs.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, rs)
	}
	return list, rows.Err()
}
