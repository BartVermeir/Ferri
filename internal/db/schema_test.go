package db

import (
	"database/sql"
	"os"
	"reflect"
	"testing"
)

// schema.sql at the project root documents the whole schema; the database
// itself is built by the migrations. It had fallen behind (audit D3). This
// test keeps the two equal: every table with the same columns.
func TestSchemaSQLMatchesMigrations(t *testing.T) {
	script, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	fromSchema, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fromSchema.Close()
	if err := execStatements(fromSchema, string(script)); err != nil {
		t.Fatalf("schema.sql: %v", err)
	}
	fromMigrations, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fromMigrations.Close()
	if err := Migrate(fromMigrations); err != nil {
		t.Fatal(err)
	}

	a, b := tableColumns(t, fromSchema), tableColumns(t, fromMigrations)
	delete(a, "schema_migrations") // the migration tracker itself
	delete(b, "schema_migrations")
	for table := range b {
		if _, ok := a[table]; !ok {
			a[table] = nil
		}
	}
	for table, cols := range a {
		if !reflect.DeepEqual(cols, b[table]) {
			t.Errorf("table %s: schema.sql has %v, the migrations have %v", table, cols, b[table])
		}
	}
}

func tableColumns(t *testing.T, d *sql.DB) map[string][]string {
	t.Helper()
	rows, err := d.Query(`SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		out[table] = append(out[table], col)
	}
	return out
}
