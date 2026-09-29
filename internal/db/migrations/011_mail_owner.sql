-- A mail about a transfer or request goes with it: when the item leaves the
-- database, its mails do too, sent or not. Mails without one (alerts, the
-- summary of a deleted transfer) only go by jobs.mail_retention_days.
ALTER TABLE mail_queue ADD COLUMN transfer_id TEXT REFERENCES transfers (id) ON DELETE CASCADE;
ALTER TABLE mail_queue ADD COLUMN request_id TEXT REFERENCES upload_requests (id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_mail_queue_transfer ON mail_queue (transfer_id) WHERE transfer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_mail_queue_request  ON mail_queue (request_id) WHERE request_id IS NOT NULL;
