package storage

import (
	"encoding/base64"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef" // 32 chars, like a real ADMIN_TOKEN

func TestEncryptDecryptRoundTrip(t *testing.T) {
	for _, plain := range []string{"", "hunter2", "wachtwoord met spaties & symbolen: €#@", strings.Repeat("x", 4096)} {
		enc, err := Encrypt(testToken, plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		got, err := Decrypt(testToken, enc)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plain {
			t.Fatalf("round trip: got %q, want %q", got, plain)
		}
	}
}

func TestDecryptWrongToken(t *testing.T) {
	enc, err := Encrypt(testToken, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt("wrong-token-wrong-token-wrong-tok", enc); err == nil {
		t.Fatal("Decrypt with wrong token succeeded, want auth failure")
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	enc, err := Encrypt(testToken, "secret")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01 // flip a bit in the GCM tag / ciphertext
	if _, err := Decrypt(testToken, base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("Decrypt of tampered blob succeeded, want auth failure")
	}
}

func TestDecryptGarbage(t *testing.T) {
	for _, bad := range []string{"", "not base64 !!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := Decrypt(testToken, bad); err == nil {
			t.Fatalf("Decrypt(%q) succeeded, want error", bad)
		}
	}
}

func TestEncryptSaltIsRandom(t *testing.T) {
	a, err := Encrypt(testToken, "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(testToken, "same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical blobs — salt/nonce not random")
	}
}
