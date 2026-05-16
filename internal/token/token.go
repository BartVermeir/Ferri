package token

import (
	"crypto/rand"
	"math/big"
)

// alphabet is base58 — no 0, O, I, l to avoid visual ambiguity.
// URL-safe without percent-encoding.
const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// tokenLen is the fixed output length for all generated tokens.
// 32 random bytes = 256 bits. ceil(256 * log(2) / log(58)) = ceil(43.7) = 44.
// Using 43 would truncate the top ~4 bits of entropy. 44 covers the full space.
// Shorter results are left-padded with the first alphabet character ('1').
const tokenLen = 44

// Generate returns a cryptographically random base58-encoded token of fixed length.
// 32 random bytes → 44 base58 characters → 256 bits of entropy.
func Generate() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("token: crypto/rand failed: " + err.Error())
	}

	n := new(big.Int).SetBytes(b)
	base := big.NewInt(int64(len(alphabet)))
	zero := big.NewInt(0)
	mod := new(big.Int)

	result := make([]byte, 0, tokenLen)
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, alphabet[mod.Int64()])
	}

	// Reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	// Pad to fixed length with the first alphabet character ('1').
	// This preserves the full entropy of the 32 random bytes when leading
	// bytes happen to be zero (big.Int drops leading zeros in SetBytes).
	for len(result) < tokenLen {
		result = append([]byte{alphabet[0]}, result...)
	}

	return string(result)
}
