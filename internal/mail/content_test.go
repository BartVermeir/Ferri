package mail

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/store"
)

// A Message-ID ending in the container hostname is a spam signal; it must
// end in the From domain instead.
func TestBuildMessage_MessageIDUsesFromDomain(t *testing.T) {
	item := store.MailItem{ToAddress: "bob@example.com", Subject: "Hi", BodyHTML: "<p>hi</p>", BodyText: "hi"}
	msg, err := buildMessage("Ferri", "ferri@example.com", item)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(buf.String(), "@example.com>") {
		t.Fatalf("expected Message-ID with the From domain, got:\n%s", buf.String())
	}
}

func TestWrap_LogoIsAbsolute(t *testing.T) {
	s := &store.Settings{CompanyName: "Example Corp", LogoURL: "/static/logo/logo.png"}
	out := Wrap(s, "https://send.example.com", "preview", "<p>body</p>")
	if !strings.Contains(out, `src="https://send.example.com/static/logo/logo.png"`) {
		t.Fatalf("expected absolute logo src, got:\n%s", out)
	}

	// Without a logo the company name is the header.
	out = Wrap(&store.Settings{CompanyName: "Example Corp"}, "https://send.example.com", "", "")
	if strings.Contains(out, "<img") || !strings.Contains(out, "Example Corp") {
		t.Fatalf("expected text header without logo, got:\n%s", out)
	}
}

func TestContrastText(t *testing.T) {
	if got := contrastText("#ffffff"); got != "#000000" {
		t.Fatalf("white background: got %s", got)
	}
	if got := contrastText("#1a1a1a"); got != "#ffffff" {
		t.Fatalf("dark background: got %s", got)
	}
}
