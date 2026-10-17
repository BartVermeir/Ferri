package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/BartVermeir/Ferri/internal/token"
)

// LoginStore keeps the sign-in links mailed to colleagues outside the
// internal network (login_tokens). Only a hash of each token is stored.
type LoginStore struct {
	db *sql.DB
}

func loginTokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Create stores a new sign-in token for email, valid for ttl, and returns it.
// It returns "" when email already got maxPerWindow tokens within window, or
// all addresses together got maxAll. Expired tokens are removed first.
func (s *LoginStore) Create(email string, ttl, window time.Duration, maxPerWindow, maxAll int) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM login_tokens WHERE expires_at <= unixepoch()`); err != nil {
		return "", err
	}
	var recent, all int
	since := time.Now().Add(-window).Unix()
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(email = ?), 0) FROM login_tokens WHERE created_at > ?`,
		email, since).Scan(&all, &recent); err != nil {
		return "", err
	}
	if recent >= maxPerWindow || all >= maxAll {
		return "", nil
	}
	tok := token.Generate()
	if _, err := tx.Exec(`INSERT INTO login_tokens (token_hash, email, expires_at) VALUES (?, ?, ?)`,
		loginTokenHash(tok), email, time.Now().Add(ttl).Unix()); err != nil {
		return "", err
	}
	return tok, tx.Commit()
}

// Peek returns the address of a token that is still valid, without using it.
// ok is false for an unknown, used or expired token.
func (s *LoginStore) Peek(tok string) (email string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT email FROM login_tokens WHERE token_hash = ? AND expires_at > unixepoch()`,
		loginTokenHash(tok)).Scan(&email)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return email, err == nil, err
}

// Consume uses a token: it returns its address and removes it, so the link
// works once. ok is false for an unknown, used or expired token.
func (s *LoginStore) Consume(tok string) (email string, ok bool, err error) {
	err = s.db.QueryRow(`DELETE FROM login_tokens WHERE token_hash = ? AND expires_at > unixepoch() RETURNING email`,
		loginTokenHash(tok)).Scan(&email)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return email, err == nil, err
}

// DeleteExpired removes expired tokens and returns how many.
func (s *LoginStore) DeleteExpired() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM login_tokens WHERE expires_at <= unixepoch()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
