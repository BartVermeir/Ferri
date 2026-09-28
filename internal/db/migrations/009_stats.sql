-- Statistics per transfer and request and purging deleted
-- items.
--
-- upload_ms:   sum of the time the TUS PATCHes of this file took, so the
--              net upload time (pauses left out). 0 = before migration 009.
-- deleted_at:  when the item got status 'deleted'; purged from the database
--              jobs.purge_deleted_after_days later. NULL = before 009, then
--              expires_at counts.
ALTER TABLE files ADD COLUMN upload_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upload_request_files ADD COLUMN upload_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE transfers ADD COLUMN deleted_at INTEGER;
ALTER TABLE upload_requests ADD COLUMN deleted_at INTEGER;

-- One row per finished download stream (a file, a resumed part of one, or a
-- ZIP), written when it ends, complete or broken off. Exactly one of
-- transfer_id / request_id is set.
CREATE TABLE IF NOT EXISTS download_streams (
    id            TEXT    NOT NULL PRIMARY KEY,
    transfer_id   TEXT    REFERENCES transfers (id) ON DELETE CASCADE,
    request_id    TEXT    REFERENCES upload_requests (id) ON DELETE CASCADE,
    who           TEXT    NOT NULL,                 -- recipient address, 'requester', 'admin'
    what          TEXT    NOT NULL,                 -- file name, or 'All files (ZIP)'
    ip_address    TEXT,
    offset_bytes  INTEGER NOT NULL DEFAULT 0,       -- Range start
    bytes_sent    INTEGER NOT NULL,
    total_bytes   INTEGER NOT NULL DEFAULT 0,       -- size of the file or the files in the ZIP
    started_at    INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_download_streams_transfer ON download_streams (transfer_id) WHERE transfer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_download_streams_request  ON download_streams (request_id) WHERE request_id IS NOT NULL;
