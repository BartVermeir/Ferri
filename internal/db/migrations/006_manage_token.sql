-- Manage link for the sender of a transfer and the requester of an upload
-- request: /manage/<manage_token> shows who downloaded what (or what was
-- received) and lets them extend or revoke it. The route only answers on the
-- internal network, like the send page.
-- NULL = created before this migration: no manage link was ever handed out.
ALTER TABLE transfers ADD COLUMN manage_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uidx_transfers_manage_token ON transfers (manage_token);
ALTER TABLE upload_requests ADD COLUMN manage_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uidx_upload_requests_manage_token ON upload_requests (manage_token);
