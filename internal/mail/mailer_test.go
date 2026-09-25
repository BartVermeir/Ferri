package mail

import (
	"bytes"
	"fmt"
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

// DEC-035: a folder can hold thousands of files; a mail names the first 20
// and counts the rest, the total still covers all of them.
func TestFileList_CapsLongLists(t *testing.T) {
	var files []FileItem
	for i := 0; i < 25; i++ {
		files = append(files, FileItem{Name: fmt.Sprintf("Series/img%02d.jpg", i), Size: 1})
	}
	text := FileListText(files)
	if strings.Count(text, "  - ") != 20 || !strings.Contains(text, "… and 5 more files") || !strings.Contains(text, "25 files, 25 B total") {
		t.Fatalf("capped text list wrong:\n%s", text)
	}
	htmlList := FileListHTML(files)
	if strings.Contains(htmlList, "img20.jpg") || !strings.Contains(htmlList, "… and 5 more files") || !strings.Contains(htmlList, "25 files") {
		t.Fatalf("capped HTML list wrong:\n%s", htmlList)
	}
}
