package token

import (
	"strings"
	"testing"
)

func TestGenerateLength(t *testing.T) {
	for i := 0; i < 200; i++ {
		tok := Generate()
		if len(tok) != tokenLen {
			t.Fatalf("token %q has length %d, want %d", tok, len(tok), tokenLen)
		}
	}
}

func TestGenerateAlphabet(t *testing.T) {
	for i := 0; i < 200; i++ {
		tok := Generate()
		for _, r := range tok {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("token %q contains char %q outside base58 alphabet", tok, r)
			}
		}
	}
}

func TestGenerateUnique(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		tok := Generate()
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}
