-- Upload history for the admin's transfer history page.
--
-- One row per stretch of uploading one file: the chunks (TUS PATCHes) of a
-- file follow each other, and a chunk that starts within a few minutes of
-- the previous one's end, from the same address, extends that row. A longer
-- pause or another address starts a new row. Exactly one of transfer_id /
-- request_id is set; the row goes with its transfer or request.
CREATE TABLE IF NOT EXISTS upload_sessions (
    id            TEXT    NOT NULL PRIMARY KEY,
    transfer_id   TEXT    REFERENCES transfers (id) ON DELETE CASCADE,
    request_id    TEXT    REFERENCES upload_requests (id) ON DELETE CASCADE,
    tus_upload_id TEXT    NOT NULL,
    who           TEXT    NOT NULL,                 -- sender address, 'uploader'
    what          TEXT    NOT NULL,                 -- file name
    ip_address    TEXT,
    offset_bytes  INTEGER NOT NULL DEFAULT 0,       -- Upload-Offset of the first chunk
    bytes_sent    INTEGER NOT NULL,
    total_bytes   INTEGER NOT NULL DEFAULT 0,       -- size of the file
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER NOT NULL,                 -- end of the last chunk
    duration_ms   INTEGER NOT NULL                  -- summed chunk time
);

CREATE INDEX IF NOT EXISTS idx_upload_sessions_tus      ON upload_sessions (tus_upload_id);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_transfer ON upload_sessions (transfer_id) WHERE transfer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_upload_sessions_request  ON upload_sessions (request_id) WHERE request_id IS NOT NULL;
