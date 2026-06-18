package store

import "database/sql"

// Stores groups all database query helpers.
// Passed via closure into handlers and jobs — no global state.
type Stores struct {
	Transfers *TransferStore
	Downloads *DownloadStore
	Requests  *RequestStore
	Mail      *MailStore
	Settings  *SettingsStore
	Files     *FilesStore
}

// New creates a Stores from a single shared *sql.DB.
func New(db *sql.DB) *Stores {
	return &Stores{
		Transfers: &TransferStore{db: db},
		Downloads: &DownloadStore{db: db},
		Requests:  &RequestStore{db: db},
		Mail:      &MailStore{db: db},
		Settings:  NewSettingsStore(db),
		Files:     &FilesStore{db: db},
	}
}
