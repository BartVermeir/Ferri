package store

import "database/sql"

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

// TransferOrRequestExists reports whether the given transfer ID or upload request token
// still has a DB record. Used to verify TUS .info file references for completed request
// files where tus_upload_id has been cleared to NULL after upload completion.
func (s *FilesStore) TransferOrRequestExists(transferID, requestToken string) (bool, error) {
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
