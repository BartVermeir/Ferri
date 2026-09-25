package tus

// Tests for enqueueTransferMails: recipients get an availability mail, the
// sender gets a confirmation carrying their own link, and the sender's link
// row never receives an availability mail itself.

import (
	"strings"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/store"
)

func TestEnqueueTransferMails_SenderGetsOwnLink(t *testing.T) {
	h, _, stores := newTestHandler(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatalf("save setting: %v", err)
	}

	res, err := stores.Transfers.Create(store.CreateTransferInput{
		Title:            "Project week 23",
		SenderName:       "Alice",
		SenderEmail:      "alice@example.com",
		ExpiresAt:        time.Now().Add(24 * time.Hour),
		Recipients:       []string{"bob@example.com"},
		Files:            []store.CreateFileInput{{OriginalName: "cut.mov", StoragePath: "p", SizeBytes: 2048}},
		NotifyRecipients: true,
		SenderLink:       true,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if err := stores.Transfers.SetFileComplete(res.Files[0].FileID, 2048); err != nil {
		t.Fatalf("complete file: %v", err)
	}

	if err := h.enqueueTransferMails(res.TransferID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	items, err := stores.Mail.FetchPending(10)
	if err != nil {
		t.Fatalf("fetch pending: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 mails (recipient + sender), got %d", len(items))
	}

	rs, _ := stores.Transfers.GetRecipients(res.TransferID)
	var bobURL, senderURL string
	for _, r := range rs {
		u := "http://example.com/dl/" + r.DownloadToken
		if r.IsSender {
			senderURL = u
		} else {
			bobURL = u
		}
	}

	byTo := map[string]store.MailItem{}
	for _, it := range items {
		byTo[it.ToAddress] = it
	}
	bob, alice := byTo["bob@example.com"], byTo["alice@example.com"]

	if !strings.Contains(bob.BodyHTML, bobURL) || !strings.Contains(bob.BodyText, bobURL) {
		t.Fatalf("recipient mail should carry the recipient's own link")
	}
	if !strings.Contains(bob.BodyHTML, "cut.mov") || !strings.Contains(bob.BodyText, "cut.mov") {
		t.Fatalf("recipient mail should list the files")
	}
	if !strings.Contains(alice.BodyHTML, senderURL) || !strings.Contains(alice.BodyText, senderURL) {
		t.Fatalf("sender confirmation should carry the sender link %s", senderURL)
	}
	if strings.Contains(alice.BodyHTML, bobURL) {
		t.Fatalf("sender confirmation must not expose a recipient's personal link")
	}
	if !strings.Contains(alice.Subject, "1 recipient") {
		t.Fatalf("sender row must not count as a recipient, subject: %q", alice.Subject)
	}
}

// A stray 'uploading' row is not part of what recipients receive.
func TestEnqueueTransferMails_ListsOnlyCompleteFiles(t *testing.T) {
	h, _, stores := newTestHandler(t)
	if err := stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	res, err := stores.Transfers.Create(store.CreateTransferInput{
		SenderEmail: "alice@example.com", ExpiresAt: time.Now().Add(24 * time.Hour),
		Recipients: []string{"bob@example.com"}, NotifyRecipients: true,
		Files: []store.CreateFileInput{
			{OriginalName: "done.mov", StoragePath: "p1", SizeBytes: 1},
			{OriginalName: "stray.mov", StoragePath: "p2", SizeBytes: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Transfers.SetFileComplete(res.Files[0].FileID, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.enqueueTransferMails(res.TransferID); err != nil {
		t.Fatal(err)
	}
	items, err := stores.Mail.FetchPending(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if !strings.Contains(it.BodyText, "done.mov") {
			t.Errorf("mail to %s misses the complete file", it.ToAddress)
		}
		if strings.Contains(it.BodyText, "stray.mov") || strings.Contains(it.BodyHTML, "stray.mov") {
			t.Errorf("mail to %s lists the incomplete file", it.ToAddress)
		}
	}
}
