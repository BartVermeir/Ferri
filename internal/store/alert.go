package store

import (
	"database/sql"
	"time"
)

// AlertStore keeps the state of the admin alerts (jobs/alerts.go), one row
// per kind in alert_state.
type AlertStore struct {
	db *sql.DB
}

// AlertState is one row of alert_state. Zero times mean NULL.
type AlertState struct {
	Since      time.Time // condition first seen; zero = not active
	LastSentAt time.Time // last alert mailed; zero = never
}

// Get returns the state of one kind; a kind without a row has zero times.
func (s *AlertStore) Get(kind string) (AlertState, error) {
	var since, sent sql.NullInt64
	err := s.db.QueryRow(`SELECT since, last_sent_at FROM alert_state WHERE kind = ?`, kind).Scan(&since, &sent)
	if err == sql.ErrNoRows {
		return AlertState{}, nil
	}
	if err != nil {
		return AlertState{}, err
	}
	var st AlertState
	if since.Valid {
		st.Since = time.Unix(since.Int64, 0)
	}
	if sent.Valid {
		st.LastSentAt = time.Unix(sent.Int64, 0)
	}
	return st, nil
}

// SetActive records that the condition holds: since is set to now unless it
// was already set, so it keeps the moment the condition started.
func (s *AlertStore) SetActive(kind string) error {
	_, err := s.db.Exec(`
		INSERT INTO alert_state (kind, since) VALUES (?, unixepoch())
		ON CONFLICT(kind) DO UPDATE SET since = COALESCE(since, excluded.since)`,
		kind,
	)
	return err
}

// Clear records that the condition is over. last_sent_at stays, so the same
// problem coming back within 24 hours does not mail again.
func (s *AlertStore) Clear(kind string) error {
	_, err := s.db.Exec(`UPDATE alert_state SET since = NULL WHERE kind = ?`, kind)
	return err
}

// MarkSent records that an alert of this kind was mailed at the given time.
func (s *AlertStore) MarkSent(kind string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO alert_state (kind, last_sent_at) VALUES (?, ?)
		ON CONFLICT(kind) DO UPDATE SET last_sent_at = excluded.last_sent_at`,
		kind, at.Unix(),
	)
	return err
}
