package config

import (
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
	c.Server.Timezone = "Europe/Brussels"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Server.Location == nil || c.Server.Location.String() != "Europe/Brussels" {
		t.Fatalf("Location = %v, want Europe/Brussels", c.Server.Location)
	}
}

func TestValidateEmptyTimezoneFallsBackToDefault(t *testing.T) {
	c := validConfig()
	c.Server.Timezone = ""
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Server.Timezone != "Europe/Brussels" || c.Server.Location == nil {
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
