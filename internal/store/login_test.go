package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/db"
)

// A sign-in token opens once: Peek leaves it, Consume uses it up.
func TestLoginStore_TokenWorksOnce(t *testing.T) {
	s := newRaceTestStores(t)
	tok, err := s.Logins.Create("alice@example.com", 15*time.Minute, 15*time.Minute, 3, 100)
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
	tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3, 100)
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
		if tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3, 100); err != nil || tok == "" {
			t.Fatalf("create %d: %q %v", i, tok, err)
		}
	}
	if tok, err := s.Logins.Create("alice@example.com", time.Hour, time.Hour, 3, 100); err != nil || tok != "" {
		t.Fatalf("fourth token: %q %v, want none", tok, err)
	}
	if tok, err := s.Logins.Create("bob@example.com", time.Hour, time.Hour, 3, 100); err != nil || tok == "" {
		t.Fatalf("other address: %q %v", tok, err)
	}
}

// All addresses together get at most maxAll links within the window.
func TestLoginStore_LimitForAllAddresses(t *testing.T) {
	s := newRaceTestStores(t)
	for i, addr := range []string{"a@example.com", "b@example.com"} {
		if tok, err := s.Logins.Create(addr, time.Hour, time.Hour, 3, 2); err != nil || tok == "" {
			t.Fatalf("link %d: %q, %v", i+1, tok, err)
		}
	}
	if tok, err := s.Logins.Create("c@example.com", time.Hour, time.Hour, 3, 2); err != nil || tok != "" {
		t.Fatalf("third address over the total: %q, %v; want no link", tok, err)
	}
}

// After CheckpointWAL a deleted row is in neither the database file nor its
// write-ahead log.
func TestCheckpointWAL_RemovesDeletedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	s := New(d)
	const marker = "marker-7f3e9c@example.com"
	if _, err := s.Logins.Create(marker, time.Hour, time.Hour, 3, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointWAL(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM login_tokens`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointWAL(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Errorf("%s still holds the deleted row", filepath.Base(f))
		}
	}
}
