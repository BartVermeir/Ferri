package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// deriveKeyInfo provides domain separation so this key can never collide with
// a key derived from the same admin token for a different purpose.
const deriveKeyInfo = "ferri-smb-password-key-v1"

// DeriveKey derives a 32-byte AES-256 key from the admin token using HKDF-SHA256.
// This means no extra env var is needed: the admin token (already required, and
// itself a 256-bit random value per deployment docs) is the root secret. HKDF
// rather than a bare hash is used for proper domain separation between any
// future keys derived from the same root secret.
func DeriveKey(adminToken string) []byte {
	key := make([]byte, 32)
	kdf := hkdf.New(sha256.New, []byte(adminToken), nil, []byte(deriveKeyInfo))
	if _, err := io.ReadFull(kdf, key); err != nil {
		// Only fails if requested output exceeds HKDF's max size (255*hash size),
		// which 32 bytes never does — kept as a hard failure rather than a silent
		// weak fallback.
		panic("storage: hkdf key derivation failed: " + err.Error())
	}
	return key
}

// Encrypt encrypts plaintext using AES-256-GCM.
// Returns a base64-encoded ciphertext that includes the nonce prefix.
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	// Seal appends ciphertext to nonce so both are stored together.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded AES-256-GCM ciphertext produced by Encrypt.
func Decrypt(key []byte, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
