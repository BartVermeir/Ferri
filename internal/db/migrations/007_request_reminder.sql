-- When the requester was reminded that nothing was uploaded yet, a day before
-- the request expires. NULL = not reminded. Extending the request clears it,
-- so the new expiry date gets its own reminder.
ALTER TABLE upload_requests ADD COLUMN reminded_at INTEGER;
