package relpath

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"img.jpg":                 "img.jpg",
		"Series/day1/img001.jpg":  "Series/day1/img001.jpg",
		"../../etc/passwd":        "etc/passwd",
		"/abs/path.mov":           "abs/path.mov",
		`C:\Users\me\clip.mov`:    "C:/Users/me/clip.mov",
		"a/./b//c/../d.txt":       "a/b/c/d.txt",
		"na\x00me\r\n.txt":        "name.txt",
		"  spaced  /  file.txt ":  "spaced/file.txt",
		"":                        "unnamed",
		"../..":                   "unnamed",
		"Séquence finale; v2.mov": "Séquence finale; v2.mov",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	long := Clean(strings.Repeat("é", 300) + "/x")
	if seg := strings.Split(long, "/")[0]; len(seg) > maxSegmentBytes || !strings.HasPrefix(seg, "é") {
		t.Errorf("long segment not cut cleanly: %d bytes", len(seg))
	}
	if got := Clean(strings.Repeat("abcdefghij/", 200)); len(got) > maxPathBytes {
		t.Errorf("path of %d bytes, want at most %d", len(got), maxPathBytes)
	}
}

func TestBaseDir(t *testing.T) {
	if Base("a/b/c.txt") != "c.txt" || Dir("a/b/c.txt") != "a/b" || Base("c.txt") != "c.txt" || Dir("c.txt") != "" {
		t.Fatal("Base/Dir wrong")
	}
}
