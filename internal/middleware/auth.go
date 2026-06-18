package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/your-org/ferri/internal/config"
)

const adminCookieName = "ferri_admin"

// AdminAuth returns middleware that validates the admin session cookie.
// On failure, redirects to /admin/login rather than returning 403,
// to avoid leaking the existence of the admin panel to external scanners.
func AdminAuth(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(adminCookieName)
			if err != nil || !validateSessionCookie(cookie.Value, cfg.Admin.Token) {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SetAdminCookie sets a signed session cookie after successful login.
// secure should match server.secure_cookies in config — true for HTTPS deployments.
func SetAdminCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	value := signedCookieValue(token, ttl)
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    value,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearAdminCookie removes the admin session cookie.
// secure must match the value used when the cookie was set.
func ClearAdminCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// signedCookieValue creates an HMAC-SHA256 signed cookie: "expiry:nonce:hmac".
// The expiry embedded in the payload matches ttl so cookie and token expire together.
func signedCookieValue(secret string, ttl time.Duration) string {
	expiry := time.Now().Add(ttl).Unix()
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	payload := fmt.Sprintf("%d:%s", expiry, hex.EncodeToString(nonce))
	mac := computeHMAC(payload, secret)
	return payload + ":" + mac
}

// validateSessionCookie verifies the cookie signature and expiry.
// Uses subtle.ConstantTimeCompare via hmac.Equal to prevent timing attacks.
func validateSessionCookie(value, secret string) bool {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	payload := parts[0] + ":" + parts[1]
	expected := computeHMAC(payload, secret)
	return hmac.Equal([]byte(expected), []byte(parts[2]))
}

func computeHMAC(payload, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
