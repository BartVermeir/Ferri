-- Sign-in links mailed to colleagues outside the internal network
-- (remote_login in config.yaml). One row per link, removed when it is used
-- or expired.
-- token_hash: sha256 of the token in the link, hex.
CREATE TABLE IF NOT EXISTS login_tokens (
    token_hash  TEXT    NOT NULL PRIMARY KEY,
    email       TEXT    NOT NULL,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    expires_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_login_tokens_email ON login_tokens (email, created_at);
