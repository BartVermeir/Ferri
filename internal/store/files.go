package store

import (
	"database/sql"
	"fmt"
)

type FilesStore struct {
	db *sql.DB
}

// AllTUSUploadIDs returns the set of all non-null TUS upload IDs recorded in the DB,
// across both transfer files and upload request files.
func (s *FilesStore) AllTUSUploadIDs() (map[string]bool, error) {
	ids := make(map[string]bool)
	rows, err := s.db.Query(`
		SELECT tus_upload_id FROM files
		WHERE tus_upload_id IS NOT NULL AND tus_upload_id != ''
		UNION ALL
		SELECT tus_upload_id FROM upload_request_files
		WHERE tus_upload_id IS NOT NULL AND tus_upload_id != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// TransferOrRequestExists reports whether the given transfer ID, upload request ID
// or (legacy .info files) upload request token still has a DB record. Used by the
// orphan scan to verify a TUS .info file whose UUID is not in the DB (an abandoned
// upload, or a legacy row) before deleting the file.
func (s *FilesStore) TransferOrRequestExists(transferID, requestID, requestToken string) (bool, error) {
	if requestID != "" {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM upload_requests WHERE id = ?`, requestID).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if transferID != "" {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM transfers WHERE id = ?`, transferID).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if requestToken != "" {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM upload_requests WHERE upload_token = ?`, requestToken).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// FileTable names one of the two file tables. Only these two values reach SQL.
type FileTable string

const (
	TransferFiles FileTable = "files"
	RequestFiles  FileTable = "upload_request_files"
)

// Unpurged is a file row marked deleted whose data may still be on storage.
//
// A deleted row keeps its tus_upload_id until the physical removal succeeded;
// MarkPurged then clears it. So status = 'deleted' with a tus_upload_id means
// "gone for the app, possibly still on disk" — the cleanup job retries those.
type Unpurged struct {
	Table       FileTable
	ID          string
	StoragePath string
	TUSUploadID string
	SizeBytes   int64
}

// ListUnpurged returns deleted file rows, from both tables, that still carry a
// tus_upload_id: failed removals, and uploads deleted before removal was fixed
// to include the flat TUS file.
func (s *FilesStore) ListUnpurged() ([]Unpurged, error) {
	rows, err := s.db.Query(`
		SELECT 'files', id, storage_path, tus_upload_id, size_bytes FROM files
		WHERE status = 'deleted' AND tus_upload_id IS NOT NULL AND tus_upload_id != ''
		UNION ALL
		SELECT 'upload_request_files', id, storage_path, tus_upload_id, size_bytes FROM upload_request_files
		WHERE status = 'deleted' AND tus_upload_id IS NOT NULL AND tus_upload_id != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Unpurged
	for rows.Next() {
		var u Unpurged
		if err := rows.Scan(&u.Table, &u.ID, &u.StoragePath, &u.TUSUploadID, &u.SizeBytes); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// MarkPurged records that a file's data is physically gone by clearing its
// tus_upload_id. Call only after storage.Manager.RemoveUpload returned nil.
func (s *FilesStore) MarkPurged(table FileTable, fileID string) error {
	var query string
	switch table {
	case TransferFiles:
		query = `UPDATE files SET tus_upload_id = NULL WHERE id = ?`
	case RequestFiles:
		query = `UPDATE upload_request_files SET tus_upload_id = NULL WHERE id = ?`
	default:
		return fmt.Errorf("MarkPurged: unknown file table %q", table)
	}
	_, err := s.db.Exec(query, fileID)
	return err
}
