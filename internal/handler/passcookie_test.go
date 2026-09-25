package handler

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Audit L1: the password cookie used to be the bcrypt hash itself: no expiry,
// and whoever saw the cookie had the hash to crack offline.
func TestPasswordCookie(t *testing.T) {
	cfg := newTestConfig()
	hash := "$2a$10$abcdefghijklmnopqrstuuvwxyzABCDEFGHIJKLMNOPQRSTUVWXY"
	now := time.Now()
	v := passwordCookieValue(cfg, "dl", "tokA", hash, now)

	if strings.Contains(v, hash) || strings.Contains(v, "$2a$") {
		t.Fatalf("cookie value %q contains the bcrypt hash", v)
	}
	if !passwordCookieValid(cfg, "dl", "tokA", hash, v, now) {
		t.Fatal("a fresh cookie is rejected")
	}
	cases := map[string]bool{
		"after the TTL":         passwordCookieValid(cfg, "dl", "tokA", hash, v, now.Add(passwordCookieTTL+time.Second)),
		"another link":          passwordCookieValid(cfg, "dl", "tokB", hash, v, now),
		"the other scope":       passwordCookieValid(cfg, "ul", "tokA", hash, v, now),
		"a changed password":    passwordCookieValid(cfg, "dl", "tokA", hash+"x", v, now),
		"the old format (hash)": passwordCookieValid(cfg, "dl", "tokA", hash, hash, now),
		"garbage":               passwordCookieValid(cfg, "dl", "tokA", hash, "nonsense", now),
	}
	exp, mac, _ := strings.Cut(v, ".")
	later, _ := strconv.ParseInt(exp, 10, 64)
	cases["an extended expiry"] = passwordCookieValid(cfg, "dl", "tokA", hash, strconv.FormatInt(later+86400, 10)+"."+mac, now)
	other := newTestConfig()
	other.Admin.Token = strings.Repeat("y", 32)
	cases["another admin token"] = passwordCookieValid(other, "dl", "tokA", hash, v, now)
	for name, ok := range cases {
		if ok {
			t.Errorf("cookie accepted for %s", name)
		}
	}
}
