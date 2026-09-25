package mail

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/store"
)

// TestBuildMessage_SubjectCRLFInjection guards against header injection: a
// Subject containing a raw CRLF must not be able to inject an extra header
// (e.g. Bcc) into the sent message.
// go-mail Q-encodes header values (RFC 2047), so CR/LF bytes never reach the
// wire unescaped.
func TestBuildMessage_SubjectCRLFInjection(t *testing.T) {
	item := store.MailItem{
		ToAddress: "victim@example.com",
		Subject:   "Hello\r\nBcc: attacker@evil.com",
		BodyHTML:  "<p>hi</p>",
		BodyText:  "hi",
	}

	msg, err := buildMessage("Ferri", "sender@example.com", item)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}

	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	raw := buf.String()

	if strings.Contains(raw, "\r\nBcc: attacker@evil.com") {
		t.Fatalf("raw message contains an injected Bcc header:\n%s", raw)
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.EqualFold(strings.TrimSpace(line), "Bcc: attacker@evil.com") {
			t.Fatalf("raw message contains an injected Bcc header line %q:\n%s", line, raw)
		}
	}
}

// TestBuildMessage_RecipientCRLFInjection ensures a recipient address
// carrying a CRLF sequence is rejected outright rather than silently
// smuggled into the header block. go-mail validates addresses via
// net/mail.ParseAddress, which rejects control characters.
func TestBuildMessage_RecipientCRLFInjection(t *testing.T) {
	item := store.MailItem{
		ToAddress: "victim@example.com>\r\nBcc: attacker@evil.com",
		Subject:   "Hello",
		BodyHTML:  "<p>hi</p>",
		BodyText:  "hi",
	}

	if _, err := buildMessage("Ferri", "sender@example.com", item); err == nil {
		t.Fatal("expected an error for a CRLF-carrying recipient address, got nil")
	}
}

// TestBuildMessage_FromFormat exercises the "Display Name <address>" From
// header, and the plain-address fallback when no display name is set.
func TestBuildMessage_FromFormat(t *testing.T) {
	item := store.MailItem{
		ToAddress: "victim@example.com",
		Subject:   "Hello",
		BodyHTML:  "<p>hi</p>",
		BodyText:  "hi",
	}

	msg, err := buildMessage("Ferri", "sender@example.com", item)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(buf.String(), `From: "Ferri" <sender@example.com>`) {
		t.Fatalf("expected a From header with display name, got:\n%s", buf.String())
	}

	msg, err = buildMessage("", "sender@example.com", item)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	buf.Reset()
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(buf.String(), "From: <sender@example.com>") {
		t.Fatalf("expected a bare From header, got:\n%s", buf.String())
	}
}
