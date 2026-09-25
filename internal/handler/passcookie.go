package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
)

// passwordCookieTTL is how long an unlocked password-protected link stays
// unlocked in a browser. The cookie itself is a session cookie; this bounds
// it on the server side too.
const passwordCookieTTL = 24 * time.Hour

// Password cookies (download page, upload request) prove that the visitor
// entered the link's password. The value is "<expiry>.<mac>", with mac =
// HMAC-SHA256 over the scope, the link token, the password's bcrypt hash and
// the expiry. It used to be the bcrypt hash itself: a bearer credential that
// never expired and handed the hash out for offline cracking (audit L1).
//
// The key is derived from the admin token, so rotating ADMIN_TOKEN logs every
// unlocked browser out. Including the bcrypt hash means a changed password
// invalidates old cookies as well.

func passwordCookieKey(cfg *config.Config) []byte {
	m := hmac.New(sha256.New, []byte(cfg.Admin.Token))
	m.Write([]byte("ferri password cookie v1"))
	return m.Sum(nil)
}

func passwordCookieMAC(cfg *config.Config, scope, tok, bcryptHash string, expiry int64) string {
	m := hmac.New(sha256.New, passwordCookieKey(cfg))
	for _, part := range []string{scope, tok, bcryptHash, strconv.FormatInt(expiry, 10)} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// passwordCookieValue returns the cookie value for a visitor who just
// entered the right password. scope is "dl" or "ul".
func passwordCookieValue(cfg *config.Config, scope, tok, bcryptHash string, now time.Time) string {
	expiry := now.Add(passwordCookieTTL).Unix()
	return strconv.FormatInt(expiry, 10) + "." + passwordCookieMAC(cfg, scope, tok, bcryptHash, expiry)
}

// passwordCookieValid checks a cookie value: well-formed, not expired, and a
// MAC that matches (compared in constant time).
func passwordCookieValid(cfg *config.Config, scope, tok, bcryptHash, value string, now time.Time) bool {
	exp, mac, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() >= expiry {
		return false
	}
	want := passwordCookieMAC(cfg, scope, tok, bcryptHash, expiry)
	return hmac.Equal([]byte(mac), []byte(want))
}
