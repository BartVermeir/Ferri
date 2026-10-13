package store

import (
	"testing"
	"time"
)

// A sign-in token opens once: Peek leaves it, Consume uses it up.
func TestLoginStore_TokenWorksOnce(t *testing.T) {
	s := newRaceTestStores(t)
	tok, err := s.Logins.Create("alice@example.com", 15*time.Minute, 15*time.Minute, 3)
	if err != nil || tok == "" {
		t.Fatalf("create: %q, %v", tok, err)
	}
	for i := 0; i < 2; i++ {
		if email, ok, err := s.Logins.Peek(tok); err != nil || !ok || email != "alice@example.com" {
			t.Fatalf("peek %d: %q %v %v", i, email, ok, err)
		}
	}
	if email, ok, err := s.Logins.Consume(tok); err != nil || !ok || email != "alice@example.com" {
		t.Fatalf("consume: %q %v %v", email, ok, err)
	}
	if _, ok, _ := s.Logins.Consume(tok); ok {
		t.Fatal("token worked twice")
	}
	if _, ok, _ := s.Logins.Peek(tok); ok {
		t.Fatal("used token still peeks")
	}
	if _, ok, _ := s.Logins.Consume("unknown"); ok {
		t.Fatal("unknown token accepted")
	}
}

// Only the hash is stored, and an expired token does not open.
func TestLoginStore_HashedAndExpires(t *testing.T) {
	s := newRaceTestStores(t)
	tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM login_tokens WHERE token_hash = ?`, tok); n != 0 {
		t.Fatal("token stored in plain text")
	}
	if _, err := s.Logins.db.Exec(`UPDATE login_tokens SET expires_at = unixepoch() - 1`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Logins.Consume(tok); ok {
		t.Fatal("expired token accepted")
	}
	if n, err := s.Logins.DeleteExpired(); err != nil || n != 1 {
		t.Fatalf("delete expired = %d, %v", n, err)
	}
}

// At most maxPerWindow tokens per address within the window; other
// addresses are not affected.
func TestLoginStore_LimitPerAddress(t *testing.T) {
	s := newRaceTestStores(t)
	for i := 0; i < 3; i++ {
		if tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3); err != nil || tok == "" {
			t.Fatalf("create %d: %q %v", i, tok, err)
		}
	}
	if tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3); err != nil || tok != "" {
		t.Fatalf("fourth token: %q %v, want none", tok, err)
	}
	if tok, err := s.Logins.Create("bob@example.com", time.Hour, time.Hour, 3); err != nil || tok == "" {
		t.Fatalf("other address: %q %v", tok, err)
	}
}
