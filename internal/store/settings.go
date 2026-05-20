package store

import (
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// Settings holds all runtime-configurable values from the settings table.
type Settings struct {
	CompanyName       string
	LogoURL           string
	PrimaryColor      string
	AccentColor       string
	BgColor           string
	FontFamily        string
	WelcomeMessage    string
	SendPageTitle     string
	DownloadPageTitle string
	MailFromName      string
	MailFromAddress   string
	NotifyOnDownload  bool
	ExpirySummary     bool

	// Storage settings
	// StorageType is "local" (default) or "smb".
	StorageType          string
	SMBHost              string
	SMBShare             string
	SMBBasePath          string
	SMBUsername          string
	// SMBPasswordEncrypted holds the AES-256-GCM encrypted password, base64-encoded.
	// Empty when no password is configured or backend is local.
	SMBPasswordEncrypted string
	SMBDomain            string
}

// SettingsStore maintains an in-memory cache of the settings table.
// Refreshed every 60 seconds and on every admin save.
type SettingsStore struct {
	db       *sql.DB
	mu       sync.RWMutex
	cached   *Settings
	fetchedAt time.Time
	ttl      time.Duration
}

func NewSettingsStore(db *sql.DB) *SettingsStore {
	return &SettingsStore{
		db:  db,
		ttl: 60 * time.Second,
	}
}

// Get returns the current settings, using the cache if fresh.
func (s *SettingsStore) Get() *Settings {
	s.mu.RLock()
	if s.cached != nil && time.Since(s.fetchedAt) < s.ttl {
		defer s.mu.RUnlock()
		return s.cached
	}
	s.mu.RUnlock()

	// Refresh
	fresh, err := s.load()
	if err != nil {
		slog.Error("failed to load settings", "error", err)
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.cached != nil {
			return s.cached // return stale rather than nil
		}
		return defaultSettings()
	}

	s.mu.Lock()
	s.cached = fresh
	s.fetchedAt = time.Now()
	s.mu.Unlock()

	return fresh
}

// Save persists a key-value pair and invalidates the cache.
func (s *SettingsStore) Save(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, unixepoch())
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = unixepoch()`,
		key, value,
	)
	if err != nil {
		return err
	}
	// Invalidate cache
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
	return nil
}

func (s *SettingsStore) load() (*Settings, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	kv := make(map[string]string)
	for rows.Next() {
		var k string
		var v sql.NullString // value column is TEXT (nullable) — NULL means "use compiled-in default"
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v.String // empty string when NULL; orDefault() handles empty strings correctly
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &Settings{
		CompanyName:       orDefault(kv["branding.company_name"], "My Organisation"),
		LogoURL:           kv["branding.logo_url"],
		PrimaryColor:      orDefault(kv["branding.primary_color"], "#000000"),
		FontFamily:        kv["branding.font_family"],
		AccentColor:       orDefault(kv["branding.accent_color"], "#f0c800"),
		BgColor:           orDefault(kv["branding.bg_color"], "#ffffff"),
		WelcomeMessage:    kv["ui.welcome_message"],
		SendPageTitle:     orDefault(kv["ui.send_page_title"], "Send files"),
		DownloadPageTitle: orDefault(kv["ui.download_page_title"], "Download files"),
		MailFromName:      orDefault(kv["mail.from_name"], "File transfer"),
		MailFromAddress:   kv["mail.from_address"],
		NotifyOnDownload:  kv["mail.notify_on_download"] != "false",
		ExpirySummary:     kv["mail.expiry_summary"] != "false",

		StorageType:          orDefault(kv["storage.type"], "local"),
		SMBHost:              kv["storage.smb_host"],
		SMBShare:             kv["storage.smb_share"],
		SMBBasePath:          kv["storage.smb_base_path"],
		SMBUsername:          kv["storage.smb_username"],
		SMBPasswordEncrypted: kv["storage.smb_password_encrypted"],
		SMBDomain:            kv["storage.smb_domain"],
	}, nil
}

func defaultSettings() *Settings {
	return &Settings{
		CompanyName:       "My Organisation",
		PrimaryColor:      "#000000",
		AccentColor:       "#f0c800",
		BgColor:           "#ffffff",
		SendPageTitle:     "Send files",
		DownloadPageTitle: "Download files",
		MailFromName:      "File transfer",
		NotifyOnDownload:  true,
		ExpirySummary:     true,
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
