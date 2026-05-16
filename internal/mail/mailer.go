package mail

// SMTP mailer for Ferri.
//
// All mail goes through the mail_queue table — this package only handles
// the SMTP send step, called by the mail job in internal/jobs/scheduler.go.
//
// Supported TLS modes (config smtp.tls):
//   starttls — connect on plain port, upgrade with STARTTLS (default, port 587)
//   tls      — connect directly with TLS (port 465)
//   none     — plain SMTP, no encryption (development only)
//
// The jordan-wright/email library is used for MIME construction.
// It handles quoted-printable encoding, multipart boundaries, and headers.

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/jordan-wright/email"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
)

// Send sends a single mail queue item via SMTP.
// Called by the mail job after fetching pending items from the queue.
// Returns an error if the send fails — the caller records the failure
// and schedules a retry with exponential backoff.
func Send(cfg *config.Config, settings *store.Settings, item store.MailItem) error {
	fromAddress := settings.MailFromAddress
	fromName := settings.MailFromName

	// Runtime settings override config defaults
	if fromAddress == "" {
		fromAddress = cfg.SMTP.FromAddress
	}
	if fromName == "" {
		fromName = cfg.SMTP.FromName
	}
	if fromAddress == "" {
		return fmt.Errorf("mail: from_address not configured")
	}

	// Build the From header: "Display Name <address>" or just "address"
	from := fromAddress
	if fromName != "" {
		from = fmt.Sprintf("%s <%s>", fromName, fromAddress)
	}

	e := email.NewEmail()
	e.From = from
	e.To = []string{item.ToAddress}
	e.Subject = item.Subject
	e.HTML = []byte(item.BodyHTML)
	e.Text = []byte(item.BodyText)

	addr := cfg.SMTPAddr()
	auth := smtpAuth(cfg)

	switch strings.ToLower(cfg.SMTP.TLS) {
	case "tls":
		// Direct TLS connection (port 465)
		tlsCfg := &tls.Config{
			ServerName: cfg.SMTP.Host,
			MinVersion: tls.VersionTLS12,
		}
		return e.SendWithTLS(addr, auth, tlsCfg)

	case "none":
		// Plain SMTP — development only
		return e.Send(addr, auth)

	default:
		// STARTTLS (default, port 587)
		tlsCfg := &tls.Config{
			ServerName: cfg.SMTP.Host,
			MinVersion: tls.VersionTLS12,
		}
		return e.SendWithStartTLS(addr, auth, tlsCfg)
	}
}

// smtpAuth builds the SMTP authentication mechanism.
// Uses PLAIN auth if credentials are configured; no auth otherwise.
func smtpAuth(cfg *config.Config) smtp.Auth {
	if cfg.SMTP.Username == "" && cfg.SMTP.Password == "" {
		return nil
	}
	// PLAIN auth: identity="", username, password, host
	return smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, cfg.SMTP.Host)
}
