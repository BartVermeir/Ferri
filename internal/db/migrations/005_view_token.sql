-- Separate token for the requester's view of received files.
-- The upload link (/ul/<upload_token>) goes to external parties; the view link
-- (/ul/<view_token>/files) only to the requester. With one token for both,
-- everyone holding the upload link could download everything uploaded through
-- it, including other uploaders' files.
-- NULL = request created before this migration: its upload_token keeps working
-- as the view token, so mails already sent stay valid until the request expires.
ALTER TABLE upload_requests ADD COLUMN view_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uidx_upload_requests_view_token ON upload_requests (view_token);
