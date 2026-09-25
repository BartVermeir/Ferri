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
// The wneessen/go-mail library is used for MIME construction. It Q-encodes
// headers (RFC 2047) and parses addresses via net/mail, so a Subject or
// address containing CR/LF cannot break out into a raw header injection —
// see mailer_test.go for a regression test of that property.

import (
	"crypto/tls"
	"fmt"
	"strings"

	gomail "github.com/wneessen/go-mail"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/store"
	"github.com/BartVermeir/Ferri/internal/token"
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

	msg, err := buildMessage(fromName, fromAddress, item)
	if err != nil {
		return fmt.Errorf("mail: build message: %w", err)
	}

	client, err := newClient(cfg)
	if err != nil {
		return fmt.Errorf("mail: create client: %w", err)
	}

	return client.DialAndSend(msg)
}

// buildMessage constructs the MIME message for a mail queue item.
// The From header is "Display Name <address>" or just "address" if no
// display name is set.
func buildMessage(fromName, fromAddress string, item store.MailItem) (*gomail.Msg, error) {
	msg := gomail.NewMsg()

	var err error
	if fromName != "" {
		err = msg.FromFormat(fromName, fromAddress)
	} else {
		err = msg.From(fromAddress)
	}
	if err != nil {
		return nil, fmt.Errorf("from address: %w", err)
	}

	if err := msg.To(item.ToAddress); err != nil {
		return nil, fmt.Errorf("to address: %w", err)
	}

	// go-mail's default Message-ID ends in os.Hostname(), which inside the
	// container is a bare container ID — not a domain. Spam filters score a
	// non-FQDN Message-ID, so use the From domain instead.
	if at := strings.LastIndex(fromAddress, "@"); at >= 0 && at < len(fromAddress)-1 {
		msg.SetMessageIDWithValue(token.Generate() + "@" + fromAddress[at+1:])
	}

	msg.Subject(item.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, item.BodyText)
	msg.AddAlternativeString(gomail.TypeTextHTML, item.BodyHTML)

	return msg, nil
}

// newClient builds the SMTP client for the configured TLS mode and auth.
func newClient(cfg *config.Config) (*gomail.Client, error) {
	opts := []gomail.Option{gomail.WithPort(cfg.SMTP.Port)}

	switch strings.ToLower(cfg.SMTP.TLS) {
	case "tls":
		// Direct TLS connection (port 465)
		opts = append(opts,
			gomail.WithSSL(),
			gomail.WithTLSConfig(&tls.Config{
				ServerName: cfg.SMTP.Host,
				MinVersion: tls.VersionTLS12,
			}),
		)

	case "none":
		// Plain SMTP — development only
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))

	default:
		// STARTTLS (default, port 587)
		opts = append(opts,
			gomail.WithTLSPolicy(gomail.TLSMandatory),
			gomail.WithTLSConfig(&tls.Config{
				ServerName: cfg.SMTP.Host,
				MinVersion: tls.VersionTLS12,
			}),
		)
	}

	if cfg.SMTP.Username != "" || cfg.SMTP.Password != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
			gomail.WithUsername(cfg.SMTP.Username),
			gomail.WithPassword(cfg.SMTP.Password),
		)
	}

	return gomail.NewClient(cfg.SMTP.Host, opts...)
}
