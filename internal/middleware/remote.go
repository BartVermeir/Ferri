package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
)

// remoteCookieName is the sign-in cookie of a colleague outside the internal
// network (remote_login). Its value is "<expiry>.<email>.<mac>", with the
// email base64url-encoded and mac = HMAC-SHA256 over email and expiry.
const remoteCookieName = "ferri_remote"

type remoteEmailKey struct{}

// IPAllowOrRemote lets a request through when its IP is in ip_allowlist, or
// when it carries a valid sign-in cookie (RemoteSession). Otherwise it goes to
// denied, which must respond with a 403. For a signed-in request from outside
// the allowlist, RemoteEmail returns the address.
func IPAllowOrRemote(cfg *config.Config, denied http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, ok := allowedIP(r, cfg.IPAllowlist, cfg.TrustedProxies)
			if ok {
				next.ServeHTTP(w, r)
				return
			}
			if email, ok := RemoteSession(r, cfg); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteEmailKey{}, email)))
				return
			}
			logBlocked(ip, r)
			denied.ServeHTTP(w, r)
		})
	}
}

// RemoteEmail returns the signed-in address of a request let through by
// IPAllowOrRemote from outside the allowlist, or "" for any other request.
func RemoteEmail(r *http.Request) string {
	email, _ := r.Context().Value(remoteEmailKey{}).(string)
	return email
}

// RemoteSession returns the address in the request's sign-in cookie. ok is
// false without a cookie, with a bad or expired one, when remote_login is off,
// or when the address's domain is no longer in remote_login.domains.
func RemoteSession(r *http.Request, cfg *config.Config) (string, bool) {
	if !cfg.RemoteLoginEnabled() {
		return "", false
	}
	c, err := r.Cookie(remoteCookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expiry {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	email := string(raw)
	if !hmac.Equal([]byte(parts[2]), []byte(remoteMAC(cfg, email, expiry))) {
		return "", false
	}
	if !cfg.RemoteLoginAllows(email) {
		return "", false
	}
	return email, true
}

// SetRemoteCookie signs email in for remote_login.session_hours.
func SetRemoteCookie(w http.ResponseWriter, cfg *config.Config, email string) {
	ttl := cfg.RemoteSessionTTL()
	expiry := time.Now().Add(ttl).Unix()
	value := strconv.FormatInt(expiry, 10) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(email)) + "." +
		remoteMAC(cfg, email, expiry)
	http.SetCookie(w, &http.Cookie{
		Name:     remoteCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   cfg.Server.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearRemoteCookie signs out.
func ClearRemoteCookie(w http.ResponseWriter, cfg *config.Config) {
	http.SetCookie(w, &http.Cookie{
		Name:     remoteCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cfg.Server.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// remoteMAC signs email and expiry with a key derived from the admin token, so
// a new ADMIN_TOKEN ends every sign-in.
func remoteMAC(cfg *config.Config, email string, expiry int64) string {
	k := hmac.New(sha256.New, []byte(cfg.Admin.Token))
	k.Write([]byte("ferri remote session v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(email))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(expiry, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
