package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

const (
	saltLen = 16 // bytes
	keyLen  = 32 // bytes → AES-256

	// Argon2id parameters (OWASP-recommended baseline for interactive use).
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
)

// deriveKey derives a 32-byte AES-256 key from the admin token and a per-ciphertext
// salt using Argon2id (memory-hard KDF). A fresh random salt is generated for every
// Encrypt call and stored alongside the ciphertext. This means a leaked ciphertext
// blob cannot be brute-forced with a precomputed table, each guess costs a full
// Argon2id derivation, and two deployments sharing the same admin token still derive
// different keys.
func deriveKey(adminToken string, salt []byte) []byte {
	return argon2.IDKey([]byte(adminToken), salt, argonTime, argonMemory, argonThreads, keyLen)
}

// Encrypt encrypts plaintext using AES-256-GCM with a key derived from adminToken
// via Argon2id. Returns a base64-encoded blob laid out as: salt || nonce || ciphertext.
func Encrypt(adminToken, plaintext string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := deriveKey(adminToken, salt)

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
	// Seal appends the ciphertext to nonce; prepend the salt so Decrypt can
	// re-derive the key. Copy into a fresh buffer to avoid aliasing salt's array.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(salt)+len(sealed))
	out = append(out, salt...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt. adminToken must match the value used to encrypt;
// the salt is read from the blob so no key needs to be passed in.
func Decrypt(adminToken, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if len(data) < saltLen {
		return "", fmt.Errorf("ciphertext too short")
	}
	salt, rest := data[:saltLen], data[saltLen:]
	key := deriveKey(adminToken, salt)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(rest) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, rest[:nonceSize], rest[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
