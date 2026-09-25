package config

import (
	"net"
	"os"
	"path/filepath"
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
		{"proxy exactly allowlisted", []string{"10.0.0.5/32"}, []string{"192.168.0.0/16", "10.0.0.5/32"}, 1},
		{"proxy inside allowlisted range", []string{"10.0.0.5/32"}, []string{"10.0.0.0/8"}, 1},
		{"no overlap", []string{"10.0.0.5/32"}, []string{"10.1.0.0/16", "192.168.1.0/24"}, 0},
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

// Audit L2: a misspelled key used to vanish without a trace. Load now names
// it, with its line, and still loads the rest.
func TestLoad_NamesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := "server:\n  base_url: \"https://files.example.com\"\n  trusted_proxy: [\"10.0.0.1/32\"]\nlimitz:\n  max_files_per_transfer: 10\n" +
		"smtp:\n  host: smtp.example.com\nadmin:\n  token: \"" + strings.Repeat("x", 32) + "\"\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BaseURL != "https://files.example.com" {
		t.Errorf("known key not loaded: base_url = %q", cfg.Server.BaseURL)
	}
	got := strings.Join(cfg.UnknownKeys, "\n")
	for _, want := range []string{"line 3: field trusted_proxy not found", "line 4: field limitz not found"} {
		if !strings.Contains(got, want) {
			t.Errorf("unknown keys %q miss %q", cfg.UnknownKeys, want)
		}
	}
}

func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if keys := unknownKeys(data); len(keys) > 0 {
		t.Fatalf("config.example.yaml has keys Ferri does not know: %q", keys)
	}
}
