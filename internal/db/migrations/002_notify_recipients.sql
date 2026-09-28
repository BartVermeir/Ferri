-- Add notify_recipients flag to transfers.
-- When 0 (link-only mode), the TUS completion handler skips sending notification emails.
-- Existing rows default to 1 (notify, matching previous behaviour).
ALTER TABLE transfers ADD COLUMN notify_recipients INTEGER NOT NULL DEFAULT 1;
