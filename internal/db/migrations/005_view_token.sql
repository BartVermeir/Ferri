-- Separate token for the requester's view of received files.
-- The upload link (/ul/<upload_token>) goes to external parties; the view link
-- (/ul/<view_token>/files) only to the requester. The upload token does not
-- open the received files.
-- NULL = request created before this column existed: its upload_token works
-- as the view token until the request expires.
ALTER TABLE upload_requests ADD COLUMN view_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uidx_upload_requests_view_token ON upload_requests (view_token);
