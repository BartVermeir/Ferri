package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

const sessSecret = "test-admin-token-at-least-32-chars-long"

func TestSessionCookieRoundTrip(t *testing.T) {
	v := signedCookieValue(sessSecret, time.Hour)
	if !validateSessionCookie(v, sessSecret) {
		t.Fatal("freshly signed cookie failed validation")
	}
}

func TestSessionCookieWrongSecret(t *testing.T) {
	v := signedCookieValue(sessSecret, time.Hour)
	if validateSessionCookie(v, "a-different-admin-token-32-characters!!") {
		t.Fatal("cookie validated against the wrong secret")
	}
}

func TestSessionCookieExpired(t *testing.T) {
	v := signedCookieValue(sessSecret, -time.Minute) // already expired
	if validateSessionCookie(v, sessSecret) {
		t.Fatal("expired cookie passed validation")
	}
}

func TestSessionCookieTampered(t *testing.T) {
	v := signedCookieValue(sessSecret, time.Hour)
	parts := strings.SplitN(v, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected cookie format %q", v)
	}
	// Push the expiry far into the future without re-signing.
	forged := "99999999999:" + parts[1] + ":" + parts[2]
	if validateSessionCookie(forged, sessSecret) {
		t.Fatal("cookie with modified expiry passed HMAC validation")
	}
}

func TestSessionCookieMalformed(t *testing.T) {
	for _, bad := range []string{"", "a", "a:b", "not:a:number:extra", "abc:def:ghi"} {
		if validateSessionCookie(bad, sessSecret) {
			t.Fatalf("malformed cookie %q passed validation", bad)
		}
	}
}

// Audit L10: the session MAC key is derived from the admin token; a cookie
// signed with the raw token (the old scheme) is not accepted.
func TestSessionCookie_KeyIsDerived(t *testing.T) {
	token := strings.Repeat("t", 32)
	good := signedCookieValue(token, time.Hour)
	if !validateSessionCookie(good, token) {
		t.Fatal("a fresh session cookie is rejected")
	}
	parts := strings.SplitN(good, ":", 3)
	raw := hmac.New(sha256.New, []byte(token))
	raw.Write([]byte(parts[0] + ":" + parts[1]))
	old := parts[0] + ":" + parts[1] + ":" + hex.EncodeToString(raw.Sum(nil))
	if validateSessionCookie(old, token) {
		t.Fatal("a cookie signed with the raw admin token is accepted")
	}
}
