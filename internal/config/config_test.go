package config

import (
	"net"
	"strings"
	"testing"
)

// validConfig returns a Config that passes validate(), as a base for mutation.
func validConfig() *Config {
	c := Defaults()
	c.Server.BaseURL = "https://send.example.com"
	c.SMTP.Host = "smtp.example.com"
	c.Admin.Token = strings.Repeat("x", 32)
	return c
}

func TestValidateOK(t *testing.T) {
	if err := validConfig().validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		errSub string
	}{
		{"missing base_url", func(c *Config) { c.Server.BaseURL = "" }, "base_url"},
		{"missing smtp host", func(c *Config) { c.SMTP.Host = "" }, "smtp.host"},
		{"missing admin token", func(c *Config) { c.Admin.Token = "" }, "admin token is required"},
		{"short admin token", func(c *Config) { c.Admin.Token = "tooshort" }, "at least 32 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.validate()
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Fatalf("validate() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func TestValidateResolvesTimezone(t *testing.T) {
	c := validConfig()
	c.Server.Timezone = "Europe/Amsterdam"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Server.Location == nil || c.Server.Location.String() != "Europe/Amsterdam" {
		t.Fatalf("Location = %v, want Europe/Amsterdam", c.Server.Location)
	}
}

func TestValidateEmptyTimezoneFallsBackToDefault(t *testing.T) {
	c := validConfig()
	c.Server.Timezone = ""
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Server.Timezone != "Europe/Amsterdam" || c.Server.Location == nil {
		t.Fatalf("empty timezone should fall back to compiled-in default; got %q / %v", c.Server.Timezone, c.Server.Location)
	}
}

func TestValidateBadTimezone(t *testing.T) {
	c := validConfig()
	c.Server.Timezone = "Mars/Olympus_Mons"
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("bad timezone should fail validation, got %v", err)
	}
}

func TestValidateZeroJobIntervalsGetDefaults(t *testing.T) {
	c := validConfig()
	c.Jobs = JobsConfig{} // all zero
	c.Limits = LimitsConfig{}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	d := Defaults()
	if c.Jobs.MailIntervalMinutes != d.Jobs.MailIntervalMinutes {
		t.Fatalf("zero MailIntervalMinutes not defaulted: got %d", c.Jobs.MailIntervalMinutes)
	}
	if c.Limits.MaxUploadBytes != d.Limits.MaxUploadBytes {
		t.Fatalf("zero MaxUploadBytes not defaulted: got %d", c.Limits.MaxUploadBytes)
	}
}

func TestProxiesInAllowlist(t *testing.T) {
	cidrs := func(in ...string) []*net.IPNet {
		var out []*net.IPNet
		for _, c := range in {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				t.Fatalf("ParseCIDR(%q): %v", c, err)
			}
			out = append(out, n)
		}
		return out
	}

	tests := []struct {
		name    string
		proxies []string
		allow   []string
		want    int
	}{
		{"proxy exactly allowlisted", []string{"172.31.0.1/32"}, []string{"192.168.0.0/16", "172.31.0.1/32"}, 1},
		{"proxy inside allowlisted range", []string{"172.31.0.1/32"}, []string{"172.16.0.0/12"}, 1},
		{"no overlap", []string{"172.31.0.1/32"}, []string{"172.20.0.0/16", "192.168.100.0/24"}, 0},
		{"no proxies", nil, []string{"10.0.0.0/8"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.TrustedProxies = cidrs(tt.proxies...)
			c.IPAllowlist = cidrs(tt.allow...)
			if got := c.ProxiesInAllowlist(); len(got) != tt.want {
				t.Fatalf("ProxiesInAllowlist() = %v, want %d entries", got, tt.want)
			}
		})
	}
}
