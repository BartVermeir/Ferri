package passcookie

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
)

func newTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Admin.Token = strings.Repeat("x", 32)
	return cfg
}

// The password cookie expires and does not contain the password hash.
func TestPasswordCookie(t *testing.T) {
	cfg := newTestConfig()
	hash := "$2a$10$abcdefghijklmnopqrstuuvwxyzABCDEFGHIJKLMNOPQRSTUVWXY"
	now := time.Now()
	v := Value(cfg, "dl", "tokA", hash, now)

	if strings.Contains(v, hash) || strings.Contains(v, "$2a$") {
		t.Fatalf("cookie value %q contains the bcrypt hash", v)
	}
	if !Valid(cfg, "dl", "tokA", hash, v, now) {
		t.Fatal("a fresh cookie is rejected")
	}
	cases := map[string]bool{
		"after the TTL":         Valid(cfg, "dl", "tokA", hash, v, now.Add(TTL+time.Second)),
		"another link":          Valid(cfg, "dl", "tokB", hash, v, now),
		"the other scope":       Valid(cfg, "ul", "tokA", hash, v, now),
		"a changed password":    Valid(cfg, "dl", "tokA", hash+"x", v, now),
		"the old format (hash)": Valid(cfg, "dl", "tokA", hash, hash, now),
		"garbage":               Valid(cfg, "dl", "tokA", hash, "nonsense", now),
	}
	exp, sig, _ := strings.Cut(v, ".")
	later, _ := strconv.ParseInt(exp, 10, 64)
	cases["an extended expiry"] = Valid(cfg, "dl", "tokA", hash, strconv.FormatInt(later+86400, 10)+"."+sig, now)
	other := newTestConfig()
	other.Admin.Token = strings.Repeat("y", 32)
	cases["another admin token"] = Valid(other, "dl", "tokA", hash, v, now)
	for name, ok := range cases {
		if ok {
			t.Errorf("cookie accepted for %s", name)
		}
	}
}
