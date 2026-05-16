package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens the SQLite database at the given path with the required PRAGMAs.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite: single writer, WAL mode for concurrent readers
	db.SetMaxOpenConns(1)

	// Each PRAGMA in its own Exec — modernc/sqlite does not reliably execute
	// multiple statements in a single Exec call.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("set pragma %q: %w", p, err)
		}
	}

	return db, nil
}

// Migrate applies any pending migrations from the embedded migrations directory.
// Migration files are applied in lexicographic order.
// Each applied migration is recorded in the schema_migrations table.
func Migrate(db *sql.DB) error {
	// Ensure schema_migrations table exists (bootstraps itself)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL DEFAULT (unixepoch())
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Read migration files
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	applied := 0
	for _, filename := range files {
		var count int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM schema_migrations WHERE filename = ?", filename,
		).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", filename, err)
		}
		if count > 0 {
			continue // already applied
		}

		content, err := migrationsFS.ReadFile("migrations/" + filename)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", filename, err)
		}

		// Run the migration and record it in a single transaction.
		// This ensures a crash between statement execution and registration
		// does not cause the migration to run again on next startup.
		err = TxFunc(db, func(tx *sql.Tx) error {
			if err := execStatements(tx, string(content)); err != nil {
				return fmt.Errorf("statements: %w", err)
			}
			_, err := tx.Exec(
				"INSERT INTO schema_migrations (filename) VALUES (?)", filename,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", filename, err)
		}

		applied++
		slog.Info("migration applied", "file", filename)
	}

	slog.Info("migrations complete", "applied", applied)
	return nil
}

// StartupHooks runs operations that must complete before the server starts.
// Currently: resets any mail_queue rows stuck in 'sending' state due to a previous crash.
func StartupHooks(db *sql.DB) error {
	// Recover mail_queue rows stuck in 'sending' for more than 10 minutes.
	// COALESCE is required: last_attempt_at is NULL on first attempt.
	// Without it, rows that crashed on their first attempt are permanently stuck.
	result, err := db.Exec(`
		UPDATE mail_queue
		SET    status          = 'pending',
		       next_attempt_at = unixepoch()
		WHERE  status          = 'sending'
		AND    COALESCE(last_attempt_at, created_at) < (unixepoch() - 600)
	`)
	if err != nil {
		return fmt.Errorf("startup hook mail_queue recovery: %w", err)
	}

	n, _ := result.RowsAffected()
	slog.Info("startup: resetting stuck mail_queue rows", "count", n)

	return nil
}

// sqlExecutor is satisfied by both *sql.DB and *sql.Tx.
type sqlExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// execStatements splits a SQL script into individual statements and executes
// each one separately. The split is semicolon-aware: it ignores semicolons
// inside single-quoted string literals and inside -- line comments.
//
// PRAGMA statements are silently skipped: they are connection-level settings
// that cannot be changed inside a transaction (journal_mode, foreign_keys),
// or would have no effect. Open() sets all required PRAGMAs before Migrate()
// is called.
//
// This is required because modernc/sqlite does not reliably execute multiple
// statements in a single Exec call.
func execStatements(ex sqlExecutor, script string) error {
	stmts := splitSQL(script)
	for _, stmt := range stmts {
		// Skip PRAGMA statements — they must not run inside a transaction.
		// journal_mode and foreign_keys are connection-level and are already
		// set by Open(). Running them inside a TX is either a no-op or an error.
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "PRAGMA") {
			continue
		}
		if _, err := ex.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(stmt, 60), err)
		}
	}
	return nil
}

// splitSQL splits a SQL script on statement-terminating semicolons,
// correctly ignoring semicolons inside string literals and line comments.
// CRLF line endings are normalised to LF first, so the parser works correctly
// on scripts committed or edited on Windows.
func splitSQL(script string) []string {
	// Normalise Windows line endings before parsing
	script = strings.ReplaceAll(script, "\r\n", "\n")

	var stmts []string
	var cur strings.Builder
	inString := false  // inside single-quoted literal
	inComment := false // inside -- line comment

	for i := 0; i < len(script); i++ {
		ch := script[i]

		switch {
		case inComment:
			// Line comment ends at newline
			if ch == '\n' {
				inComment = false
			}
			// Do not write comment characters to the statement buffer

		case inString:
			cur.WriteByte(ch)
			if ch == '\'' {
				// Check for escaped quote ('')
				if i+1 < len(script) && script[i+1] == '\'' {
					cur.WriteByte(script[i+1])
					i++
				} else {
					inString = false
				}
			}

		case ch == '-' && i+1 < len(script) && script[i+1] == '-':
			// Start of line comment — skip the rest of the line
			inComment = true
			i++ // skip second '-'

		case ch == '\'':
			inString = true
			cur.WriteByte(ch)

		case ch == ';':
			stmt := strings.TrimSpace(cur.String())
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			cur.Reset()

		default:
			cur.WriteByte(ch)
		}
	}

	// Handle any trailing statement without a terminating semicolon
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}

	return stmts
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TxFunc executes fn inside a transaction. If fn returns an error, the
// transaction is rolled back; otherwise it is committed.
//
// Note: store packages define a local txFunc to avoid importing this package
// (which would create an import cycle via the migrations embed). Both
// implementations are identical. If the logic changes, update both.
func TxFunc(db *sql.DB, fn func(tx *sql.Tx) error) error {
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

// NullTime converts *time.Time to sql.NullInt64 (Unix epoch seconds).
func NullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// FromNullTime converts sql.NullInt64 to *time.Time.
func FromNullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0)
	return &t
}
