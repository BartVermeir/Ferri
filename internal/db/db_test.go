package db

import (
	"reflect"
	"testing"
)

func TestSplitSQL(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   []string
	}{
		{
			name:   "simple statements",
			script: "CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);",
			want:   []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name:   "semicolon inside string literal is not a separator",
			script: "INSERT INTO t VALUES ('a;b');\nSELECT 1;",
			want:   []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"},
		},
		{
			name:   "escaped quote inside string",
			script: "INSERT INTO t VALUES ('it''s; fine');",
			want:   []string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			name:   "line comment with semicolon is stripped",
			script: "SELECT 1; -- trailing; comment\nSELECT 2;",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "CRLF line endings",
			script: "SELECT 1;\r\nSELECT 2;\r\n",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "trailing statement without semicolon",
			script: "SELECT 1;\nSELECT 2",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "empty and whitespace-only input",
			script: "  \n\t ;; \n",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQL(tt.script)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitSQL()\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}
