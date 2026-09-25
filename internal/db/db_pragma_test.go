package db

import (
	"path/filepath"
	"testing"
)

// Audit L3: every connection, not just the first, must have foreign keys on,
// the busy timeout set and WAL mode. The pool allows one connection; the test
// forces it to be replaced and checks the new one.
func TestOpen_PragmasOnEveryConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	check := func(when string) {
		t.Helper()
		var fk, busy int
		var mode string
		if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy != 5000 || mode != "wal" {
			t.Fatalf("%s: foreign_keys = %d, busy_timeout = %d, journal_mode = %q", when, fk, busy, mode)
		}
	}
	check("first connection")

	d.SetMaxIdleConns(0) // the next query opens a fresh connection
	d.SetMaxIdleConns(2)
	check("replacement connection")

	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*_pragma*")); len(matches) > 0 {
		t.Fatalf("the DSN options ended up in a file name: %v", matches)
	}
}
