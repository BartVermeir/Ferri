package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/your-org/ferri/internal/token"
)

type TransferStore struct {
	db *sql.DB
}

// Transfer represents a row in the transfers table.
type Transfer struct {
	ID           string
	Title        string
	Message      string
	SenderName   string
	SenderEmail  string
	PasswordHash sql.NullString
	Status       string
	ExpiresAt    time.Time
	ActivatedAt  sql.NullInt64 // nullable epoch seconds
	ExpiredAt    sql.NullInt64 // nullable epoch seconds
	CreatedAt    time.Time
}

// File represents a row in the files table.
type File struct {
	ID                string
	TransferID        string
	OriginalName      string
	StoragePath       string
	SizeBytes         int64
	MimeType          sql.NullString
	TUSUploadID       sql.NullString
	TUSLastActivityAt sql.NullInt64
	Status            string
	CreatedAt         time.Time
}

// Recipient represents a row in the recipients table.
type Recipient struct {
	ID              string
	TransferID      string
	Email           string
	DownloadToken   string
	NotifiedAt      sql.NullInt64
	FirstDownloadAt sql.NullInt64
	DownloadCount   int
	CreatedAt       time.Time
}

// CreateTransferInput holds all data needed to create a transfer atomically.
type CreateTransferInput struct {
	Title        string
	Message      string
	SenderName   string
	SenderEmail  string
	PasswordHash string // empty = no password
	ExpiresAt    time.Time
	Recipients   []string // email addresses
	Files        []CreateFileInput
}

type CreateFileInput struct {
	OriginalName string
	StoragePath  string
	SizeBytes    int64
}

// CreateTransferResult holds the IDs generated during transfer creation.
type CreateTransferResult struct {
	TransferID string
	Recipients []RecipientResult
	Files      []FileResult
}

type RecipientResult struct {
	Email         string
	RecipientID   string
	DownloadToken string
}

type FileResult struct {
	FileID string
}

// Create creates a transfer with all associated files and recipients in a single transaction.
func (s *TransferStore) Create(input CreateTransferInput) (*CreateTransferResult, error) {
	result := &CreateTransferResult{}

	err := txFunc(s.db, func(tx *sql.Tx) error {
		transferID := token.Generate()
		result.TransferID = transferID

		var passwordHash sql.NullString
		if input.PasswordHash != "" {
			passwordHash = sql.NullString{String: input.PasswordHash, Valid: true}
		}

		_, err := tx.Exec(`
			INSERT INTO transfers (id, title, message, sender_name, sender_email,
			                       password_hash, status, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
			transferID,
			input.Title,
			input.Message,
			input.SenderName,
			input.SenderEmail,
			passwordHash,
			input.ExpiresAt.Unix(),
		)
		if err != nil {
			return fmt.Errorf("insert transfer: %w", err)
		}

		for _, f := range input.Files {
			fileID := token.Generate()
			_, err := tx.Exec(`
				INSERT INTO files (id, transfer_id, original_name, storage_path,
				                   size_bytes, status)
				VALUES (?, ?, ?, ?, ?, 'uploading')`,
				fileID, transferID, f.OriginalName, f.StoragePath, f.SizeBytes,
			)
			if err != nil {
				return fmt.Errorf("insert file: %w", err)
			}
			result.Files = append(result.Files, FileResult{FileID: fileID})
		}

		for _, email := range input.Recipients {
			recipientID := token.Generate()
			downloadToken := token.Generate()
			_, err := tx.Exec(`
				INSERT INTO recipients (id, transfer_id, email, download_token)
				VALUES (?, ?, ?, ?)`,
				recipientID, transferID, email, downloadToken,
			)
			if err != nil {
				return fmt.Errorf("insert recipient: %w", err)
			}
			result.Recipients = append(result.Recipients, RecipientResult{
				Email:         email,
				RecipientID:   recipientID,
				DownloadToken: downloadToken,
			})
		}

		return nil
	})

	return result, err
}

// GetByDownloadToken looks up a transfer and its files by a recipient's download token.
// Returns nil if not found, expired, or not active.
func (s *TransferStore) GetByDownloadToken(tok string) (*Transfer, *Recipient, []File, error) {
	var t Transfer
	var r Recipient
	var expiresAt, tCreatedAt, rCreatedAt int64

	err := s.db.QueryRow(`
		SELECT t.id, t.title, t.message, t.sender_name, t.sender_email,
		       t.password_hash, t.status, t.expires_at, t.activated_at,
		       t.expired_at, t.created_at,
		       r.id, r.transfer_id, r.email, r.download_token, r.notified_at,
		       r.first_download_at, r.download_count, r.created_at
		FROM recipients r
		JOIN transfers t ON t.id = r.transfer_id
		WHERE r.download_token = ?
		  AND t.status = 'active'
		  AND t.expires_at > unixepoch()`,
		tok,
	).Scan(
		&t.ID, &t.Title, &t.Message, &t.SenderName, &t.SenderEmail,
		&t.PasswordHash, &t.Status, &expiresAt, &t.ActivatedAt,
		&t.ExpiredAt, &tCreatedAt,
		&r.ID, &r.TransferID, &r.Email, &r.DownloadToken, &r.NotifiedAt,
		&r.FirstDownloadAt, &r.DownloadCount, &rCreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}

	t.ExpiresAt = time.Unix(expiresAt, 0)
	t.CreatedAt = time.Unix(tCreatedAt, 0)
	r.CreatedAt = time.Unix(rCreatedAt, 0)

	files, err := s.filesByTransferID(t.ID)
	if err != nil {
		return nil, nil, nil, err
	}

	return &t, &r, files, nil
}

// GetFileByID returns a single file row.
func (s *TransferStore) GetFileByID(fileID string) (*File, error) {
	var f File
	var createdAt int64
	err := s.db.QueryRow(`
		SELECT id, transfer_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM files WHERE id = ?`, fileID,
	).Scan(
		&f.ID, &f.TransferID, &f.OriginalName, &f.StoragePath, &f.SizeBytes,
		&f.MimeType, &f.TUSUploadID, &f.TUSLastActivityAt, &f.Status, &createdAt,
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

// TryActivate atomically sets the transfer to 'active' if all its files are complete.
// Returns true if this call caused the activation. Race-safe: only one goroutine wins.
func (s *TransferStore) TryActivate(transferID string) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE transfers
		SET    status       = 'active',
		       activated_at = unixepoch()
		WHERE  id     = ?
		AND    status = 'pending'
		AND    (SELECT COUNT(*) FROM files
		        WHERE transfer_id = ? AND status != 'complete') = 0`,
		transferID, transferID,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// SetFileComplete marks a file as complete and records the final size.
// tus_upload_id is kept so the download handler can locate the file via
// tusd's storage layout (<storage_path>/<tus_upload_id>).
func (s *TransferStore) SetFileComplete(fileID string, sizeBytes int64) error {
	_, err := s.db.Exec(`
		UPDATE files
		SET    status     = 'complete',
		       size_bytes = ?
		WHERE  id = ?`,
		sizeBytes, fileID,
	)
	return err
}

// UpdateTUSActivity records the latest TUS PATCH activity timestamp for a file.
func (s *TransferStore) UpdateTUSActivity(fileID string) error {
	_, err := s.db.Exec(
		`UPDATE files SET tus_last_activity_at = unixepoch() WHERE id = ?`, fileID,
	)
	return err
}

// GetExpired returns transfers that are active and past their expiry time.
func (s *TransferStore) GetExpired() ([]Transfer, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, sender_name, sender_email,
		       password_hash, status, expires_at, activated_at, expired_at, created_at
		FROM transfers
		WHERE status = 'active' AND expires_at < unixepoch()`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// SetExpired marks a transfer as expired.
func (s *TransferStore) SetExpired(transferID string) error {
	_, err := s.db.Exec(`
		UPDATE transfers
		SET status = 'expired', expired_at = unixepoch()
		WHERE id = ?`, transferID,
	)
	return err
}

// GetForCleanup returns expired transfers whose cleanup grace period has passed.
func (s *TransferStore) GetForCleanup(graceHours int) ([]Transfer, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, sender_name, sender_email,
		       password_hash, status, expires_at, activated_at, expired_at, created_at
		FROM transfers
		WHERE status = 'expired'
		  AND expires_at < (unixepoch() - ? * 3600)`,
		graceHours,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// MarkFilesDeleted nulls download_events.file_id and marks files deleted — in one transaction.
// Must be called AFTER physical file deletion.
func (s *TransferStore) MarkFilesDeleted(transferID string) error {
	return txFunc(s.db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			UPDATE download_events
			SET file_id = NULL
			WHERE file_id IN (SELECT id FROM files WHERE transfer_id = ?)`,
			transferID,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE files SET status = 'deleted' WHERE transfer_id = ?`, transferID,
		)
		return err
	})
}

// SumFileSizes returns total size_bytes for complete files. Call BEFORE os.RemoveAll.
func (s *TransferStore) SumFileSizes(transferID string) (int64, error) {
	var total int64
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(size_bytes), 0)
		FROM files
		WHERE transfer_id = ? AND status = 'complete'`,
		transferID,
	).Scan(&total)
	return total, err
}

// SoftDelete marks a transfer as deleted (admin action).
func (s *TransferStore) SoftDelete(transferID string) error {
	_, err := s.db.Exec(
		`UPDATE transfers SET status = 'deleted' WHERE id = ?`, transferID,
	)
	return err
}

// GetStalled returns files idle longer than stallHours.
// COALESCE catches files that never received a chunk (tus_last_activity_at IS NULL).
func (s *TransferStore) GetStalled(stallHours int) ([]File, error) {
	rows, err := s.db.Query(`
		SELECT id, transfer_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM files
		WHERE status = 'uploading'
		  AND COALESCE(tus_last_activity_at, created_at) < (unixepoch() - ? * 3600)`,
		stallHours,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFiles(rows)
}

// MarkFileDeleted marks a single file as deleted.
func (s *TransferStore) MarkFileDeleted(fileID string) error {
	_, err := s.db.Exec(`UPDATE files SET status = 'deleted' WHERE id = ?`, fileID)
	return err
}

// ListActive returns pending/active transfers for the admin dashboard.
func (s *TransferStore) ListActive(limit int) ([]Transfer, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, sender_name, sender_email,
		       password_hash, status, expires_at, activated_at, expired_at, created_at
		FROM transfers
		WHERE status IN ('pending','active')
		ORDER BY created_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// ListAll returns all transfers for the admin panel.
func (s *TransferStore) ListAll(limit int) ([]Transfer, error) {
	rows, err := s.db.Query(`
		SELECT id, title, message, sender_name, sender_email,
		       password_hash, status, expires_at, activated_at, expired_at, created_at
		FROM transfers
		ORDER BY created_at DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// GetRecipients returns all recipients for a transfer.
func (s *TransferStore) GetRecipients(transferID string) ([]Recipient, error) {
	rows, err := s.db.Query(`
		SELECT id, transfer_id, email, download_token, notified_at,
		       first_download_at, download_count, created_at
		FROM recipients
		WHERE transfer_id = ?
		ORDER BY email`,
		transferID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recipients []Recipient
	for rows.Next() {
		var r Recipient
		var createdAt int64
		if err := rows.Scan(
			&r.ID, &r.TransferID, &r.Email, &r.DownloadToken,
			&r.NotifiedAt, &r.FirstDownloadAt, &r.DownloadCount, &createdAt,
		); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		recipients = append(recipients, r)
	}
	return recipients, rows.Err()
}

// MarkRecipientNotified sets notified_at on a recipient row.
func (s *TransferStore) MarkRecipientNotified(recipientID string) error {
	_, err := s.db.Exec(
		`UPDATE recipients SET notified_at = unixepoch() WHERE id = ?`, recipientID,
	)
	return err
}

// ValidateForTUS checks that a transfer exists, is pending, and not expired.
func (s *TransferStore) ValidateForTUS(transferID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM transfers
		WHERE id = ? AND status = 'pending' AND expires_at > unixepoch()`,
		transferID,
	).Scan(&count)
	return count > 0, err
}


// CreateFileRow inserts a new file row for a transfer, called from the TUS
// PreUploadCreateCallback before any bytes are written.
func (s *TransferStore) CreateFileRow(fileID, transferID, originalName, storagePath string, sizeBytes int64) error {
	_, err := s.db.Exec(`
		INSERT INTO files (id, transfer_id, original_name, storage_path, size_bytes, status)
		VALUES (?, ?, ?, ?, ?, 'uploading')`,
		fileID, transferID, originalName, storagePath, sizeBytes,
	)
	return err
}

// SetTUSUploadID records the tusd-generated upload ID on the file row.
// Called from the handleCreated hook after tusd creates the upload resource.
func (s *TransferStore) SetTUSUploadID(fileID, tusUploadID string) error {
	result, err := s.db.Exec(
		`UPDATE files SET tus_upload_id = ? WHERE id = ?`, tusUploadID, fileID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("SetTUSUploadID: no rows updated for file_id=%s", fileID)
	}
	return nil
}

// GetFileIDByTUSID looks up the Ferri file ID by the tusd upload ID.
// Used in the post-PATCH hook to update tus_last_activity_at.
// Returns empty string if not found.
func (s *TransferStore) GetFileIDByTUSID(tusUploadID string) (string, error) {
	var fileID string
	err := s.db.QueryRow(
		`SELECT id FROM files WHERE tus_upload_id = ?`, tusUploadID,
	).Scan(&fileID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return fileID, err
}

// GetByID returns a transfer by its ID.
// Used by the TUS completion handler to get sender info for mail enqueue.
func (s *TransferStore) GetByID(transferID string) (*Transfer, error) {
	var t Transfer
	var expiresAt, createdAt int64
	err := s.db.QueryRow(`
		SELECT id, title, message, sender_name, sender_email,
		       password_hash, status, expires_at, activated_at, expired_at, created_at
		FROM transfers WHERE id = ?`, transferID,
	).Scan(
		&t.ID, &t.Title, &t.Message, &t.SenderName, &t.SenderEmail,
		&t.PasswordHash, &t.Status, &expiresAt, &t.ActivatedAt,
		&t.ExpiredAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = time.Unix(expiresAt, 0)
	t.CreatedAt = time.Unix(createdAt, 0)
	return &t, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *TransferStore) filesByTransferID(transferID string) ([]File, error) {
	rows, err := s.db.Query(`
		SELECT id, transfer_id, original_name, storage_path, size_bytes,
		       mime_type, tus_upload_id, tus_last_activity_at, status, created_at
		FROM files WHERE transfer_id = ? AND status = 'complete'
		ORDER BY created_at`,
		transferID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFiles(rows)
}

// scanTransfers scans rows from the transfers table.
// expires_at and created_at are INTEGER (Unix epoch) in SQLite — scan into int64,
// then convert to time.Time. Scanning directly into time.Time would fail at runtime.
func scanTransfers(rows *sql.Rows) ([]Transfer, error) {
	var list []Transfer
	for rows.Next() {
		var t Transfer
		var expiresAt, createdAt int64
		if err := rows.Scan(
			&t.ID, &t.Title, &t.Message, &t.SenderName, &t.SenderEmail,
			&t.PasswordHash, &t.Status, &expiresAt, &t.ActivatedAt,
			&t.ExpiredAt, &createdAt,
		); err != nil {
			return nil, err
		}
		t.ExpiresAt = time.Unix(expiresAt, 0)
		t.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, t)
	}
	return list, rows.Err()
}

// scanFiles scans rows from the files table.
// created_at is INTEGER in SQLite — scan into int64, convert to time.Time.
func scanFiles(rows *sql.Rows) ([]File, error) {
	var list []File
	for rows.Next() {
		var f File
		var createdAt int64
		if err := rows.Scan(
			&f.ID, &f.TransferID, &f.OriginalName, &f.StoragePath, &f.SizeBytes,
			&f.MimeType, &f.TUSUploadID, &f.TUSLastActivityAt, &f.Status, &createdAt,
		); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(createdAt, 0)
		list = append(list, f)
	}
	return list, rows.Err()
}

// txFunc executes fn inside a transaction, rolling back on error.
// Note: db.TxFunc in the db package does the same — this local version
// avoids an import cycle since store packages don't import db directly.
func txFunc(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
