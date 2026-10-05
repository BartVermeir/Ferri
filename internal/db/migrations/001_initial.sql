-- =============================================================
-- File sharing tool — SQLite schema
-- =============================================================
-- Conventions:
--   - All IDs: base58-encoded 32-byte random strings (opaque tokens)
--   - All timestamps: Unix epoch seconds (INTEGER), named *_at
--   - Deleting sets status = 'deleted'; the row is purged once its data is off storage
--   - Foreign keys enforced (PRAGMA foreign_keys = ON at connection open)
-- =============================================================

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;


-- -------------------------------------------------------------
-- transfers
-- One row per outgoing transfer created by an internal user.
--
-- activated_at: set when all files finish uploading (status → active).
-- expired_at:   set when the expiry job processes this transfer.
-- Both are NULL until that transition occurs.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS transfers (
    id              TEXT    NOT NULL PRIMARY KEY,
    title           TEXT    NOT NULL DEFAULT '',
    message         TEXT    NOT NULL DEFAULT '',
    sender_name     TEXT    NOT NULL DEFAULT '',
    sender_email    TEXT    NOT NULL DEFAULT '',
    password_hash   TEXT,
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending','active','expired','deleted')),
                            -- pending:  TUS upload(s) still in progress
                            -- active:   all files uploaded, link is live
                            -- expired:  past expires_at, expiry job has run
                            -- deleted:  admin deleted manually
    expires_at      INTEGER NOT NULL,
    activated_at    INTEGER,                        -- set when status → active
    expired_at      INTEGER,                        -- set when status → expired
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_transfers_status       ON transfers (status);
CREATE INDEX IF NOT EXISTS idx_transfers_expires_at   ON transfers (expires_at);
CREATE INDEX IF NOT EXISTS idx_transfers_sender_email ON transfers (sender_email);


-- -------------------------------------------------------------
-- files
-- One row per file within a transfer.
--
-- Files of one transfer can finish uploading concurrently, so the
-- transfer goes to 'active' in a single UPDATE...WHERE with a
-- subquery (TransferStore.TryActivate), not a SELECT then UPDATE.
--
-- tus_last_activity_at: updated on every TUS PATCH request.
-- Used by the stalled-upload cleanup job to detect abandoned uploads.
-- Stalled uploads are cleaned up after stall_timeout_hours regardless
-- of the parent transfer's expiry date; the transfer itself is not
-- marked expired.
--
-- The cleanup query uses COALESCE(tus_last_activity_at, created_at):
-- tus_last_activity_at is NULL when no chunk ever arrived.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS files (
    id                   TEXT    NOT NULL PRIMARY KEY,
    transfer_id          TEXT    NOT NULL REFERENCES transfers (id) ON DELETE CASCADE,
    original_name        TEXT    NOT NULL,
    storage_path         TEXT    NOT NULL,          -- logical path, e.g. "transfers/<transfer_id>/<file_id>"
    size_bytes           INTEGER NOT NULL DEFAULT 0,
    mime_type            TEXT,                      -- detected server-side, not trusted from client
    tus_upload_id        TEXT    UNIQUE,            -- tusd upload id; retained after completion
                                                    -- (the data lives at <storage_root>/<tus_upload_id>)
    tus_last_activity_at INTEGER,                   -- epoch of last TUS PATCH; NULL before first chunk
    status               TEXT    NOT NULL DEFAULT 'uploading'
                                 CHECK (status IN ('uploading','complete','deleted')),
    created_at           INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_files_transfer_id  ON files (transfer_id);
CREATE INDEX IF NOT EXISTS idx_files_tus_id       ON files (tus_upload_id) WHERE tus_upload_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_files_stalled      ON files (tus_last_activity_at) WHERE status = 'uploading';


-- -------------------------------------------------------------
-- recipients
-- One row per email address a transfer is sent to.
-- Each recipient gets their own download_token.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS recipients (
    id                  TEXT    NOT NULL PRIMARY KEY,
    transfer_id         TEXT    NOT NULL REFERENCES transfers (id) ON DELETE CASCADE,
    email               TEXT    NOT NULL,
    download_token      TEXT    NOT NULL UNIQUE,
    notified_at         INTEGER,                    -- when the availability mail was sent
    first_download_at   INTEGER,                    -- NULL until first download
    download_count      INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_recipients_transfer_id    ON recipients (transfer_id);
CREATE INDEX IF NOT EXISTS idx_recipients_download_token ON recipients (download_token);
CREATE UNIQUE INDEX IF NOT EXISTS uidx_recipient_per_transfer
    ON recipients (transfer_id, email);


-- -------------------------------------------------------------
-- download_events
-- Append-only audit log of every file download.
-- Used as the source for expiry summary mails.
--
-- No CASCADE on file_id: the events outlive their files, the
-- expiry summary mail is built from them. file_id is a nullable
-- FK, set to NULL when the referenced file row is deleted.
--
-- original_name is stored here as well as on files: the expiry
-- summary mail shows file names after the files are deleted.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS download_events (
    id              TEXT    NOT NULL PRIMARY KEY,
    recipient_id    TEXT    NOT NULL REFERENCES recipients (id) ON DELETE CASCADE,
    file_id         TEXT    REFERENCES files (id) ON DELETE SET NULL,  -- nullable: file may be deleted
    original_name   TEXT    NOT NULL,               -- denormalised from files.original_name
                                                    -- so the expiry summary still works after file deletion
    ip_address      TEXT,
    user_agent      TEXT,
    downloaded_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_download_events_recipient  ON download_events (recipient_id);
CREATE INDEX IF NOT EXISTS idx_download_events_file       ON download_events (file_id) WHERE file_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_download_events_at         ON download_events (downloaded_at);


-- -------------------------------------------------------------
-- upload_requests
-- Inverse of transfers: an internal user requests an external
-- party to upload files via a token link.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS upload_requests (
    id                  TEXT    NOT NULL PRIMARY KEY,
    title               TEXT    NOT NULL DEFAULT '',
    message             TEXT    NOT NULL DEFAULT '',
    requester_name      TEXT    NOT NULL DEFAULT '',
    requester_email     TEXT    NOT NULL DEFAULT '',
    upload_token        TEXT    NOT NULL UNIQUE,
    password_hash       TEXT,
    max_files           INTEGER,                    -- NULL = unlimited
    max_total_bytes     INTEGER,                    -- NULL = use global config limit
    status              TEXT    NOT NULL DEFAULT 'open'
                                CHECK (status IN ('open','completed','expired','deleted')),
    expires_at          INTEGER NOT NULL,
    completed_at        INTEGER,                    -- set when uploader clicks "done"
    expired_at          INTEGER,                    -- set when expiry job processes this
    created_at          INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_upload_requests_status  ON upload_requests (status);
CREATE INDEX IF NOT EXISTS idx_upload_requests_expiry  ON upload_requests (expires_at);
CREATE INDEX IF NOT EXISTS idx_upload_requests_token   ON upload_requests (upload_token);


-- -------------------------------------------------------------
-- upload_request_files
-- Files received via an upload request link.
--
-- Same columns as `files`: a column added to `files` is added
-- here too.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS upload_request_files (
    id                   TEXT    NOT NULL PRIMARY KEY,
    upload_request_id    TEXT    NOT NULL REFERENCES upload_requests (id) ON DELETE CASCADE,
    original_name        TEXT    NOT NULL,
    storage_path         TEXT    NOT NULL,
    size_bytes           INTEGER NOT NULL DEFAULT 0,
    mime_type            TEXT,
    tus_upload_id        TEXT    UNIQUE,            -- retained after completion, same as files.tus_upload_id
    tus_last_activity_at INTEGER,
    status               TEXT    NOT NULL DEFAULT 'uploading'
                                 CHECK (status IN ('uploading','complete','deleted')),
    created_at           INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_urf_request_id ON upload_request_files (upload_request_id);
CREATE INDEX IF NOT EXISTS idx_urf_tus_id     ON upload_request_files (tus_upload_id) WHERE tus_upload_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_urf_stalled    ON upload_request_files (tus_last_activity_at) WHERE status = 'uploading';


-- -------------------------------------------------------------
-- mail_queue
-- Persistent outbound mail queue with exponential backoff retry.
--
-- status flow:  pending -> sending -> sent
--                                  -> failed  (after max_attempts)
--
-- The 'sending' status prevents double-sends when the mail job
-- overlaps itself. On startup, the application resets rows stuck
-- in 'sending' for more than 10 minutes back to 'pending'
-- (db/db.go startup hook):
--   UPDATE mail_queue SET status='pending', next_attempt_at=unixepoch()
--   WHERE status='sending'
--   AND COALESCE(last_attempt_at, created_at) < (unixepoch() - 600)
--
-- COALESCE: last_attempt_at is NULL until a send attempt completes
-- or fails.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mail_queue (
    id              TEXT    NOT NULL PRIMARY KEY,
    to_address      TEXT    NOT NULL,
    subject         TEXT    NOT NULL,
    body_html       TEXT    NOT NULL,
    body_text       TEXT    NOT NULL DEFAULT '',    -- plain-text fallback
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending','sending','sent','failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 5,
    last_attempt_at INTEGER,
    next_attempt_at INTEGER NOT NULL DEFAULT (unixepoch()),
    error_message   TEXT,                           -- last SMTP error, for diagnostics
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_mail_queue_pending ON mail_queue (next_attempt_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_mail_queue_sent_at ON mail_queue (last_attempt_at)
    WHERE status = 'sent';


-- -------------------------------------------------------------
-- schema_migrations
-- Tracks which migration files have been applied.
-- The application reads files from internal/db/migrations/ in
-- lexicographic order and applies any not yet present here.
-- Never modify or delete rows from this table manually.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
    filename    TEXT    NOT NULL PRIMARY KEY,   -- e.g. "001_initial.sql"
    applied_at  INTEGER NOT NULL DEFAULT (unixepoch())
);


-- -------------------------------------------------------------
-- settings
-- Key-value store for runtime configuration (admin UI).
-- All values stored as TEXT; the application casts as needed.
-- Keys are namespaced: "branding.logo_url", "mail.from_name", etc.
-- -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settings (
    key         TEXT    NOT NULL PRIMARY KEY,
    value       TEXT,                               -- NULL = use compiled-in default
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

INSERT OR IGNORE INTO settings (key, value) VALUES
    ('branding.company_name',   'My Organisation'),
    ('branding.logo_url',       ''),
    ('branding.primary_color',  '#000000'),
    ('branding.accent_color',   '#f0c800'),
    ('branding.bg_color',       '#ffffff'),
    ('ui.welcome_message',      ''),
    ('ui.send_page_title',      'Send files'),
    ('ui.download_page_title',  'Download files'),
    ('mail.from_name',          'File transfer'),
    ('mail.from_address',       ''),
    ('mail.notify_on_download', 'true'),
    ('mail.expiry_summary',     'true');
