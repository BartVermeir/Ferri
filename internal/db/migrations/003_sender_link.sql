-- Add is_sender flag to recipients.
-- A transfer that notifies recipients by mail also gets one extra recipient
-- row for the sender (is_sender = 1), so the sender's confirmation mail can
-- carry a link of its own. That row never gets an availability mail, never
-- triggers a download notification, is labelled separately in the expiry
-- summary and is not counted as a recipient.
-- Existing rows get 0 (a real recipient).
ALTER TABLE recipients ADD COLUMN is_sender INTEGER NOT NULL DEFAULT 0;
