package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/config"
)

func remoteTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Admin.Token = strings.Repeat("x", 32)
	cfg.IPAllowlist = []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	cfg.RemoteLogin.Domains = []string{"example.com"}
	return cfg
}

// signInCookie returns the cookie SetRemoteCookie sets for email.
func signInCookie(t *testing.T, cfg *config.Config, email string) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	SetRemoteCookie(rr, cfg, email)
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	return cookies[0]
}

func TestIPAllowOrRemote(t *testing.T) {
	cfg := remoteTestConfig(t)
	var seen string
	h := IPAllowOrRemote(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RemoteEmail(r)
	}))

	do := func(from string, c *http.Cookie) int {
		seen = "-"
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = from + ":4000"
		if c != nil {
			r.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}

	good := signInCookie(t, cfg, "alice@example.com")
	if code := do("10.1.2.3", nil); code != http.StatusOK || seen != "" {
		t.Fatalf("inside: %d, email %q", code, seen)
	}
	if code := do("10.1.2.3", good); code != http.StatusOK || seen != "" {
		t.Fatalf("inside with cookie: %d, email %q, want no remote email", code, seen)
	}
	if code := do("203.0.113.9", nil); code != http.StatusForbidden {
		t.Fatalf("outside without cookie: %d", code)
	}
	if code := do("203.0.113.9", good); code != http.StatusOK || seen != "alice@example.com" {
		t.Fatalf("outside with cookie: %d, email %q", code, seen)
	}

	forged := *good
	forged.Value = strings.Replace(good.Value, ".", "9.", 1) // later expiry, same mac
	if code := do("203.0.113.9", &forged); code != http.StatusForbidden {
		t.Fatalf("forged expiry: %d", code)
	}
	other := signInCookie(t, cfg, "bob@example.com")
	swapped := *good
	swapped.Value = strings.Join([]string{
		strings.Split(good.Value, ".")[0], strings.Split(other.Value, ".")[1], strings.Split(good.Value, ".")[2],
	}, ".")
	if code := do("203.0.113.9", &swapped); code != http.StatusForbidden {
		t.Fatalf("swapped address: %d", code)
	}

	// A domain taken off the list, a new admin token, or remote login turned
	// off ends the sign-in.
	cfg.RemoteLogin.Domains = []string{"example.org"}
	if code := do("203.0.113.9", good); code != http.StatusForbidden {
		t.Fatalf("domain removed: %d", code)
	}
	cfg.RemoteLogin.Domains = nil
	if code := do("203.0.113.9", good); code != http.StatusForbidden {
		t.Fatalf("remote login off: %d", code)
	}
	cfg.RemoteLogin.Domains = []string{"example.com"}
	cfg.Admin.Token = strings.Repeat("y", 32)
	if code := do("203.0.113.9", good); code != http.StatusForbidden {
		t.Fatalf("new admin token: %d", code)
	}
}

func TestRemoteSession_Expired(t *testing.T) {
	cfg := remoteTestConfig(t)
	cfg.RemoteLogin.SessionHours = 1
	c := signInCookie(t, cfg, "alice@example.com")
	_, rest, _ := strings.Cut(c.Value, ".")
	c.Value = "1." + rest
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	if _, ok := RemoteSession(r, cfg); ok {
		t.Fatal("expired sign-in accepted")
	}
}

func TestRemoteCookie_Attributes(t *testing.T) {
	cfg := remoteTestConfig(t)
	cfg.Server.SecureCookies = true
	c := signInCookie(t, cfg, "alice@example.com")
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 24*3600 {
		t.Fatalf("cookie attributes: %+v", c)
	}
	rr := httptest.NewRecorder()
	ClearRemoteCookie(rr, cfg)
	if got := rr.Result().Cookies(); len(got) != 1 || got[0].MaxAge >= 0 {
		t.Fatalf("clear: %+v", got)
	}
}
