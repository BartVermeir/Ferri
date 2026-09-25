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
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/store"
	"github.com/BartVermeir/Ferri/internal/token"
)

// Sender sends mail queue items over one SMTP connection, opened on the
// first mail and reused for the rest of the batch. One connection per mail
// (connect, TLS, login, send, quit) capped the queue at about 600 mails an
// hour (audit O5). After a failed send the connection is dropped and the
// next mail opens a fresh one, so one bad mail cannot poison the others.
// Not safe for concurrent use; Close when the batch is done.
type Sender struct {
	cfg         *config.Config
	fromName    string
	fromAddress string
	client      *gomail.Client // nil until the first mail, and after a failure
	dials       int            // connections opened, for tests
}

// NewSender prepares a Sender. Runtime settings override the config's
// from name and address.
func NewSender(cfg *config.Config, settings *store.Settings) *Sender {
	s := &Sender{cfg: cfg, fromAddress: settings.MailFromAddress, fromName: settings.MailFromName}
	if s.fromAddress == "" {
		s.fromAddress = cfg.SMTP.FromAddress
	}
	if s.fromName == "" {
		s.fromName = cfg.SMTP.FromName
	}
	return s
}

// Send sends one item. Returns an error if the send fails — the caller
// records the failure and schedules a retry with exponential backoff.
func (s *Sender) Send(item store.MailItem) error {
	if s.fromAddress == "" {
		return fmt.Errorf("mail: from_address not configured")
	}
	msg, err := buildMessage(s.fromName, s.fromAddress, item)
	if err != nil {
		return fmt.Errorf("mail: build message: %w", err)
	}
	if s.client == nil {
		client, err := newClient(s.cfg)
		if err != nil {
			return fmt.Errorf("mail: create client: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = client.DialWithContext(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("mail: connect: %w", err)
		}
		s.client = client
		s.dials++
	}
	if err := s.client.Send(msg); err != nil {
		s.Close()
		return err
	}
	return nil
}

// Close ends the SMTP session, if one is open.
func (s *Sender) Close() {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
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
