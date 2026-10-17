// Package relpath cleans the file names browsers send with an upload. Files
// from a folder keep their path inside it ("Series/day1/img001.jpg");
// that path ends up as a ZIP entry name, so it must never climb out of the
// archive or carry an absolute or drive path.
package relpath

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSegmentBytes = 255  // a file or folder name, as most file systems allow
	maxPathBytes    = 1024 // the whole path
)

// Clean returns name as a safe relative path: backslashes become slashes;
// empty, "." and ".." segments, control characters and characters that change
// how the rest displays (bidi controls, zero-width space) are dropped; each
// segment and the whole path are cut to a sane length. What is left of
// "../../etc/passwd" is "etc/passwd", of "C:\Users\x.mov" it is "C:/Users/x.mov"
// ("C:" is an ordinary folder name inside a ZIP). An empty result is "unnamed".
func Clean(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	var segs []string
	for _, seg := range strings.Split(name, "/") {
		seg = strings.TrimSpace(strings.Map(func(r rune) rune {
			if dropped(r) {
				return -1
			}
			return r
		}, seg))
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segs = append(segs, truncate(seg, maxSegmentBytes))
	}
	out := truncate(strings.Join(segs, "/"), maxPathBytes)
	out = strings.Trim(out, "/")
	if out == "" {
		return "unnamed"
	}
	return out
}

// dropped reports whether Clean removes r: C0 and C1 control characters, bidi
// controls ("invoice\u202Efdp.exe" displays as "invoiceexe.pdf"), zero-width
// space, word joiner and byte order mark.
func dropped(r rune) bool {
	switch {
	case r < 0x20 || (r >= 0x7f && r <= 0x9f):
		return true
	case unicode.Is(unicode.Bidi_Control, r):
		return true
	case r == 0x200B || r == 0x2060 || r == 0xFEFF:
		return true
	}
	return false
}

// Base returns the last segment of a cleaned path: the file name.
func Base(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// Dir returns everything before the file name, or "" for a file at the top.
func Dir(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
