package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full deploy-time configuration.
// Loaded once at startup from config.yaml; never mutated after loading.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	DB      DBConfig      `yaml:"db"`

	// IPAllowlist is parsed from ip_allowlist CIDR ranges.
	// Used by the IPAllow middleware to restrict transfer creation.
	IPAllowlist []*net.IPNet `yaml:"-"`
	RawAllowlist []string    `yaml:"ip_allowlist"`

	SMTP   SMTPConfig   `yaml:"smtp"`
	Admin  AdminConfig  `yaml:"admin"`

	ExpiryOptions []ExpiryOption `yaml:"expiry_options"`

	Limits LimitsConfig `yaml:"limits"`
	Jobs   JobsConfig   `yaml:"jobs"`
}

type ServerConfig struct {
	Host                    string   `yaml:"host"`
	Port                    int      `yaml:"port"`
	BaseURL                 string   `yaml:"base_url"`
	TrustedProxies          []string `yaml:"trusted_proxies"`
	ShutdownTimeoutSeconds  int      `yaml:"shutdown_timeout_seconds"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

type SMTPConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"` // overridden by SMTP_PASSWORD env var
	TLS         string `yaml:"tls"`       // starttls | tls | none
	FromAddress string `yaml:"from_address"`
	FromName    string `yaml:"from_name"`
}

type AdminConfig struct {
	Token          string `yaml:"token"`         // overridden by ADMIN_TOKEN env var
	SessionTTLHours int   `yaml:"session_ttl_hours"`
}

type ExpiryOption struct {
	Label string `yaml:"label"`
	Hours int    `yaml:"hours"`
}

type LimitsConfig struct {
	MaxUploadBytes       int64 `yaml:"max_upload_bytes"`
	MaxFilesPerTransfer  int   `yaml:"max_files_per_transfer"`
}

type JobsConfig struct {
	ExpiryIntervalMinutes  int `yaml:"expiry_interval_minutes"`
	CleanupGraceHours      int `yaml:"cleanup_grace_hours"`
	MailIntervalMinutes    int `yaml:"mail_interval_minutes"`
	StallTimeoutHours      int `yaml:"stall_timeout_hours"`
	CleanupIntervalHours   int `yaml:"cleanup_interval_hours"`
	MailRetentionDays      int `yaml:"mail_retention_days"`
}

// Defaults returns a Config with all default values pre-filled.
func Defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Host:                   "0.0.0.0",
			Port:                   8080,
			ShutdownTimeoutSeconds: 300, // matches stop_grace_period in docker-compose.yml
		},
		Storage: StorageConfig{Path: "/data/storage"},
		DB:      DBConfig{Path: "/data/app.db"},
		SMTP: SMTPConfig{
			Port: 587,
			TLS:  "starttls",
		},
		Admin: AdminConfig{
			SessionTTLHours: 8,
		},
		ExpiryOptions: []ExpiryOption{
			{Label: "1 day", Hours: 24},
			{Label: "1 week", Hours: 168},
			{Label: "2 weeks", Hours: 336},
			{Label: "4 weeks", Hours: 672},
		},
		Limits: LimitsConfig{
			MaxUploadBytes:      644_245_094_400, // 600 GB
			MaxFilesPerTransfer: 50,
		},
		Jobs: JobsConfig{
			ExpiryIntervalMinutes: 60,
			CleanupGraceHours:     24,
			MailIntervalMinutes:   2,
			StallTimeoutHours:     48,
			CleanupIntervalHours:  6,
			MailRetentionDays:     90,
		},
	}
}

// Load reads config from the given path, applies environment variable overrides,
// and validates the result.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open config: %w", err)
		}
		defer f.Close()

		if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// Environment variable overrides for secrets
	if v := os.Getenv("SMTP_PASSWORD"); v != "" {
		cfg.SMTP.Password = v
	}
	if v := os.Getenv("ADMIN_TOKEN"); v != "" {
		cfg.Admin.Token = v
	}

	// Parse CIDR ranges
	for _, cidr := range cfg.RawAllowlist {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid ip_allowlist entry %q: %w", cidr, err)
		}
		cfg.IPAllowlist = append(cfg.IPAllowlist, network)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Server.BaseURL == "" {
		return fmt.Errorf("server.base_url is required")
	}
	if c.SMTP.Host == "" {
		return fmt.Errorf("smtp.host is required")
	}
	if c.Admin.Token == "" {
		return fmt.Errorf("admin token is required (set ADMIN_TOKEN env var)")
	}

	// yaml.v3 decodes missing keys as zero values, overriding our defaults.
	// A zero interval causes time.NewTicker(0) to panic at startup.
	// Re-apply defaults for any job interval that ended up as zero.
	d := Defaults()
	if c.Jobs.MailIntervalMinutes <= 0 {
		c.Jobs.MailIntervalMinutes = d.Jobs.MailIntervalMinutes
	}
	if c.Jobs.ExpiryIntervalMinutes <= 0 {
		c.Jobs.ExpiryIntervalMinutes = d.Jobs.ExpiryIntervalMinutes
	}
	if c.Jobs.CleanupIntervalHours <= 0 {
		c.Jobs.CleanupIntervalHours = d.Jobs.CleanupIntervalHours
	}
	if c.Jobs.CleanupGraceHours <= 0 {
		c.Jobs.CleanupGraceHours = d.Jobs.CleanupGraceHours
	}
	if c.Jobs.StallTimeoutHours <= 0 {
		c.Jobs.StallTimeoutHours = d.Jobs.StallTimeoutHours
	}
	if c.Jobs.MailRetentionDays <= 0 {
		c.Jobs.MailRetentionDays = d.Jobs.MailRetentionDays
	}
	if c.Admin.SessionTTLHours <= 0 {
		c.Admin.SessionTTLHours = d.Admin.SessionTTLHours
	}
	if c.Limits.MaxUploadBytes <= 0 {
		c.Limits.MaxUploadBytes = d.Limits.MaxUploadBytes
	}
	if c.Limits.MaxFilesPerTransfer <= 0 {
		c.Limits.MaxFilesPerTransfer = d.Limits.MaxFilesPerTransfer
	}
	if c.Server.ShutdownTimeoutSeconds <= 0 {
		c.Server.ShutdownTimeoutSeconds = d.Server.ShutdownTimeoutSeconds
	}
	if c.SMTP.Port <= 0 {
		c.SMTP.Port = d.SMTP.Port
	}

	return nil
}

// SessionTTL returns the admin session TTL as a time.Duration.
func (c *Config) SessionTTL() time.Duration {
	hours := c.Admin.SessionTTLHours
	if hours <= 0 {
		hours = 8
	}
	return time.Duration(hours) * time.Hour
}

// ExpiryDuration returns the expiry duration for a given number of hours.
func ExpiryDuration(hours int) time.Duration {
	return time.Duration(hours) * time.Hour
}

// portStr returns the SMTP port as a string for use in net.Dial.
func (c *Config) SMTPAddr() string {
	return c.SMTP.Host + ":" + strconv.Itoa(c.SMTP.Port)
}
