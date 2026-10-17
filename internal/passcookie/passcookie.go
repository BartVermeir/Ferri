// Package passcookie signs and checks the cookies that prove a visitor entered
// a link's password (download page, upload request).
package passcookie

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
)

// TTL is how long an unlocked password-protected link stays unlocked in a
// browser. The cookie is a session cookie; the server also rejects it after
// this.
const TTL = 24 * time.Hour

// UploadName is the name of the upload request password cookie for tok. It is
// set for /ul/<tok> and for /tus, where uploads are created.
func UploadName(tok string) string {
	return "ferri_ul_auth_" + tok
}

// Password cookies (download page, upload request) prove that the visitor
// entered the link's password. The value is "<expiry>.<mac>", with mac =
// HMAC-SHA256 over the scope, the link token, the password's bcrypt hash and
// the expiry.
//
// The key is derived from the admin token: a new ADMIN_TOKEN invalidates every
// cookie, and a changed password invalidates the cookies of that link.

func key(cfg *config.Config) []byte {
	m := hmac.New(sha256.New, []byte(cfg.Admin.Token))
	m.Write([]byte("ferri password cookie v1"))
	return m.Sum(nil)
}

func mac(cfg *config.Config, scope, tok, bcryptHash string, expiry int64) string {
	m := hmac.New(sha256.New, key(cfg))
	for _, part := range []string{scope, tok, bcryptHash, strconv.FormatInt(expiry, 10)} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Value returns the cookie value for a visitor who just entered the right
// password. scope is "dl" or "ul".
func Value(cfg *config.Config, scope, tok, bcryptHash string, now time.Time) string {
	expiry := now.Add(TTL).Unix()
	return strconv.FormatInt(expiry, 10) + "." + mac(cfg, scope, tok, bcryptHash, expiry)
}

// Valid checks a cookie value: well-formed, not expired, and a MAC that
// matches (compared in constant time).
func Valid(cfg *config.Config, scope, tok, bcryptHash, value string, now time.Time) bool {
	exp, got, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() >= expiry {
		return false
	}
	want := mac(cfg, scope, tok, bcryptHash, expiry)
	return hmac.Equal([]byte(got), []byte(want))
}
