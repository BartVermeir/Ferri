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
	Alerts    *AlertStore
	Stats     *StatsStore
	Logins    *LoginStore

	db *sql.DB
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
		Alerts:    &AlertStore{db: db},
		Stats:     &StatsStore{db: db},
		Logins:    &LoginStore{db: db},
		db:        db,
	}
}

// CheckpointWAL writes the write-ahead log into the database file and empties
// it, so deleted rows are no longer in app.db-wal.
func (s *Stores) CheckpointWAL() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// execChanged turns the result of an UPDATE into whether it changed a row.
func execChanged(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
