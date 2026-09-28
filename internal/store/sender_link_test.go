package store

// Tests for CreateTransferInput.SenderLink: the sender gets one extra
// recipient row flagged is_sender, unless they are already a recipient.

import (
	"testing"
	"time"
)

func TestCreate_SenderLinkAddsFlaggedRow(t *testing.T) {
	stores := newRaceTestStores(t)

	res, err := stores.Transfers.Create(CreateTransferInput{
		SenderEmail: "alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"bob@example.com"},
		SenderLink:  true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Recipients) != 1 {
		t.Fatalf("result should only list real recipients, got %d", len(res.Recipients))
	}

	rs, err := stores.Transfers.GetRecipients(res.TransferID)
	if err != nil {
		t.Fatalf("get recipients: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("expected recipient + sender row, got %d", len(rs))
	}
	if rs[0].IsSender || rs[0].Email != "bob@example.com" {
		t.Fatalf("first row should be the real recipient, got %+v", rs[0])
	}
	if !rs[1].IsSender || rs[1].Email != "alice@example.com" {
		t.Fatalf("second row should be the sender link, got %+v", rs[1])
	}
}

func TestCreate_SenderLinkSkippedWhenSenderIsRecipient(t *testing.T) {
	stores := newRaceTestStores(t)

	res, err := stores.Transfers.Create(CreateTransferInput{
		SenderEmail: "Alice@example.com",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Recipients:  []string{"alice@example.com", "bob@example.com"},
		SenderLink:  true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rs, err := stores.Transfers.GetRecipients(res.TransferID)
	if err != nil {
		t.Fatalf("get recipients: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("expected no extra sender row, got %d rows", len(rs))
	}
	for _, r := range rs {
		if r.IsSender {
			t.Fatalf("no row should be flagged is_sender, got %+v", r)
		}
	}
}
