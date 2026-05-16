# architecture.md

This document describes the complete technical architecture of this file sharing application. It is intended as the primary reference for any developer, maintainer, or AI agent working on the codebase. Read this before reading any source code.

For *why* decisions were made, see `DECISIONS.md`. This document focuses on *what* the system is and *how* it works.

---

## Table of contents

1. [System overview](#1-system-overview)
2. [Repository layout](#2-repository-layout)
3. [Request routing](#3-request-routing)
4. [Component descriptions](#4-component-descriptions)
5. [Data flows](#5-data-flows)
6. [Database](#6-database)
7. [File storage](#7-file-storage)
8. [Mail](#8-mail)
9. [Configuration](#9-configuration)
10. [Background jobs](#10-background-jobs)
11. [Security layers](#11-security-layers)
12. [Docker layout](#12-docker-layout)

---

## 1. System overview

This is **Ferri** — a self-hosted file transfer application. It has two primary workflows:

**Outgoing transfer (Send):** An internal user uploads files and generates a download link. External recipients download via a token URL, no account required.

**Upload request (Request):** An internal user generates an upload link and sends it to an external party. The external party uploads files; the internal user is notified.

**Topology:**

```
Internet
  │
  ▼
Reverse proxy (Caddy/nginx) — TLS termination, port 443
  │
  ▼ localhost:8080
Go application binary
  ├── SQLite database          (metadata, tokens, mail queue, settings)
  ├── Storage path mount       (actual files, NFS/ZFS/local)
  └── SMTP relay               (outbound mail only, via mail_queue table)
```

The application is a single Go binary. It has no runtime dependencies beyond the filesystem mount and an SMTP relay. It listens only on localhost; the reverse proxy handles all external traffic.

---

## 2. Repository layout

```
/
├── cmd/
│   └── server/
│       └── main.go              # Entry point. Wires config, DB, router, jobs.
├── internal/
│   ├── config/
│   │   └── config.go            # Load and validate config.yaml + env overrides
│   ├── db/
│   │   ├── db.go                # Open connection, run migrations, startup hooks
│   │   └── migrations/
│   │       └── 001_initial.sql  # Schema (= schema.sql at project root)
│   ├── handler/
│   │   ├── send.go              # POST /send — create transfer
│   │   ├── download.go          # GET /dl/:token — download page + file serve
│   │   ├── request.go           # POST /request — create upload request
│   │   ├── upload.go            # GET /ul/:token — upload request page
│   │   ├── admin.go             # GET/POST /admin/* — admin UI
│   │   └── health.go            # GET /health — liveness probe
│   ├── middleware/
│   │   ├── ipallow.go           # IP allowlist check for create endpoints
│   │   ├── auth.go              # Admin session cookie validation
│   │   ├── settings.go          # Inject runtime settings into request context
│   │   └── recovery.go          # Panic recovery + structured error logging
│   ├── tus/
│   │   └── handler.go           # Embed tusd, validate tokens, wire DB callbacks
│   ├── mail/
│   │   ├── mailer.go            # SMTP client: reads mail_queue, sends, updates status
│   │   └── templates/
│   │       ├── transfer_available.html
│   │       ├── transfer_confirm.html
│   │       ├── download_notify.html
│   │       ├── expiry_summary.html
│   │       └── upload_complete.html
│   ├── jobs/
│   │   └── scheduler.go         # Ticker-based background job runner
│   ├── store/
│   │   ├── transfer.go          # DB queries for transfers + files + recipients
│   │   ├── download.go          # DB queries for download_events
│   │   ├── request.go           # DB queries for upload_requests + upload_request_files
│   │   ├── mail.go              # DB queries for mail_queue
│   │   └── settings.go          # DB queries + in-memory cache for settings
│   └── token/
│       └── token.go             # Generate base58 tokens (crypto/rand)
├── web/
│   ├── templates/
│   │   ├── base.html
│   │   ├── send.html
│   │   ├── download.html
│   │   ├── upload.html
│   │   ├── password.html
│   │   └── admin/
│   │       ├── dashboard.html
│   │       ├── transfers.html
│   │       ├── mail.html        # Failed mail queue overview + retry UI
│   │       └── settings.html
│   └── static/
│       ├── tus.min.js
│       ├── upload.js
│       └── style.css
├── schema.sql
├── config.example.yaml
├── docker-compose.yml
├── Dockerfile
├── DECISIONS.md
├── architecture.md
├── operations.md
└── update-guide.md
```

All files under `web/` are embedded into the binary at compile time via `//go:embed`.

---

## 3. Request routing

### Middleware stack (applied globally)

1. `middleware/recovery.go` — catches panics, logs them, returns 500
2. `middleware/settings.go` — loads runtime settings from DB cache, injects into context
3. Standard request logging

### Public routes (no IP restriction)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/dl/:token` | `handler/download.go` | Download page or password prompt |
| POST | `/dl/:token` | `handler/download.go` | Password submission |
| GET | `/dl/:token/file/:file_id` | `handler/download.go` | Stream file to browser |
| GET | `/ul/:token` | `handler/upload.go` | Upload request page or password prompt |
| POST | `/ul/:token` | `handler/upload.go` | Password submission |
| POST | `/ul/:token/complete` | `handler/upload.go` | Uploader signals they are done |
| POST | `/tus/*` | `tus/handler.go` | TUS upload chunks — token validated before first byte (see §4) |
| GET | `/health` | `handler/health.go` | Liveness probe |
| GET | `/static/*` | embedded FS | CSS, JS, fonts |

### IP-restricted routes (internal network only)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/` | `handler/send.go` | Send form |
| POST | `/send` | `handler/send.go` | Create transfer |
| GET | `/request` | `handler/request.go` | Upload request form |
| POST | `/request` | `handler/request.go` | Create upload request |

### Admin routes (IP-restricted + session cookie)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/admin/login` | `handler/admin.go` | Login form |
| POST | `/admin/login` | `handler/admin.go` | Validate token, set session cookie |
| GET | `/admin` | `handler/admin.go` | Dashboard |
| GET | `/admin/transfers` | `handler/admin.go` | List all transfers |
| POST | `/admin/transfers/:id/delete` | `handler/admin.go` | Soft-delete a transfer |
| GET | `/admin/mail` | `handler/admin.go` | Mail queue — failed/pending overview |
| POST | `/admin/mail/:id/retry` | `handler/admin.go` | Reset failed mail to pending (attempts=0) |
| POST | `/admin/mail/:id/delete` | `handler/admin.go` | Remove a mail from the queue |
| GET | `/admin/settings` | `handler/admin.go` | Runtime settings form |
| POST | `/admin/settings` | `handler/admin.go` | Save settings |
| POST | `/admin/logout` | `handler/admin.go` | Clear session cookie |

---

## 4. Component descriptions

### config

Loaded once at startup from `config.yaml` (path set via `CONFIG_PATH` env var) with environment variable overrides for secrets. The config struct is passed into every component that needs it. It is never mutated after startup.

Secrets that should come from environment variables:
- `SMTP_PASSWORD`
- `ADMIN_TOKEN`
- `LITESTREAM_*` (backup credentials)

### db

Opens a single SQLite connection with `WAL` journal mode and `foreign_keys = ON`. Runs pending migrations on startup. Uses `modernc/sqlite` (pure Go, no CGo).

**Startup hook — recover stuck mail_queue rows:**

Before the job scheduler starts, `db.go` runs:

```sql
UPDATE mail_queue
SET status = 'pending', next_attempt_at = unixepoch()
WHERE status = 'sending'
  AND COALESCE(last_attempt_at, created_at) < (unixepoch() - 600);
```

`COALESCE` is required here. On a first send attempt, `last_attempt_at` is still NULL — it is only set after a completed or failed attempt. `NULL < anything` evaluates to NULL in SQL, which is treated as false. Without the COALESCE, any row that was set to `sending` and then crashed before the first attempt completes would be permanently stuck. `created_at` is the correct fallback: if the row has been in `sending` for more than 10 minutes since it was created, it is safe to reset.

This recovers any rows left in `sending` state by a previous process that crashed mid-send. Without this, those mails would be permanently stuck and silently never retried.

### store

Thin wrappers around raw SQL queries. No ORM. Functions take a `*sql.DB` and return typed Go structs. Named parameters throughout.

The settings store maintains an in-memory cache, refreshed every 60 seconds and on every admin save.

### handler

Handlers receive `config`, `store`, and `mailer` via closure. No global state.

Handlers render Go `html/template` files. The base template injects branding as CSS custom properties:

```html
<style>
  :root {
    --color-primary: {{ .Settings.PrimaryColor }};
    --color-accent:  {{ .Settings.AccentColor }};
    --color-bg:      {{ .Settings.BgColor }};
  }
</style>
```

### tus

Wraps `tusd`'s HTTP handler. The TUS endpoint is public because external parties must be able to upload via request links. This makes token validation at the TUS layer critical.

**Token validation on upload create (`POST /tus/`):**

The browser JS sends either `X-Transfer-Id` or `X-Upload-Request-Token` as a TUS extension header. The `PreUploadCreateCallback` fires before any data is written:

1. Reads the header to determine context (transfer vs upload request)
2. Looks up the ID/token in the database
3. Verifies: exists, status is `pending`/`open`, `expires_at > now()`
4. Checks `Upload-Length` header against `limits.max_upload_bytes` from config.
   The TUS client sends the total file size at upload-create time. If it exceeds
   the configured limit, the callback returns HTTP 413 (Request Entity Too Large)
   before any storage is allocated. This is the only reliable place to enforce
   the size limit — checking after upload would waste disk space and bandwidth.
5. If any check fails: returns the appropriate HTTP error — no bytes written,
   no storage allocated, no database rows created

**On every TUS PATCH (chunk received):**

Updates `tus_last_activity_at` on the file row. Used by the stalled-upload cleanup job.

**On TUS completion (`UploadFinisher` hook) — race condition handling:**

When multiple files in one transfer finish concurrently, a naive "check if all complete, then update transfer" has a race: two goroutines both check, both see incomplete, neither activates the transfer.

The correct implementation uses a single atomic query:

```sql
UPDATE transfers
SET status = 'active', activated_at = unixepoch()
WHERE id = ?
  AND status = 'pending'
  AND (SELECT COUNT(*) FROM files
       WHERE transfer_id = ? AND status != 'complete') = 0;
```

This UPDATE only succeeds for exactly one goroutine — the one that happens to run after the last file completes. The affected row count is checked: if it is 1, this goroutine won the race and is responsible for inserting the notification mails. If it is 0, another goroutine already handled it, and this goroutine does nothing further.

This pattern requires no locks and no separate transactions.

**Stalled upload cleanup vs active uploads:**

The stalled-upload job removes partial files after `stall_timeout_hours` of inactivity — **regardless of the parent transfer's expiry date**. A 400GB upload abandoned after 10 minutes should not occupy NFS storage for 4 weeks just because the transfer has a 4-week expiry.

When a stalled file is cleaned up, the parent transfer is **not** marked expired. It stays `pending`, allowing the user to start a fresh upload using the same transfer form if they want to retry.

**Mail inserts after TUS completion:**

The UploadFinisher inserts rows into `mail_queue`. It does not send directly.

### mail

Mail is never sent synchronously. The flow is always:

```
event occurs (TUS complete / download / expiry)
  → INSERT INTO mail_queue (status='pending')

mail job (every 2 minutes)
  → fetch pending rows
  → send via SMTP
  → update status
```

**Retry schedule (exponential backoff):**

| Attempt | Delay before next retry |
|---|---|
| 1st failure | 2 minutes |
| 2nd failure | 8 minutes |
| 3rd failure | 30 minutes |
| 4th failure | 2 hours |
| 5th failure | marked `failed`, no more retries |

**Failed mail recovery:**

Failed mails are visible in the admin UI at `/admin/mail`. The operator can:
- See the SMTP error message for each failed mail
- Retry individually: `POST /admin/mail/:id/retry` resets `attempts = 0`, `status = 'pending'`
- Delete: `POST /admin/mail/:id/delete`

The admin dashboard shows a badge with the count of failed mails so the operator notices without actively checking.

### admin authentication

**Login flow:**

1. User navigates to `/admin/login` (IP-restricted)
2. Submits a form with the admin token
3. Server compares using `subtle.ConstantTimeCompare` to prevent timing attacks
4. On success: sets a signed `HttpOnly; Secure; SameSite=Strict` session cookie
5. Cookie TTL: configurable via `admin.session_ttl_hours`, default 8 hours

**Session validation:**

The cookie value is an HMAC-SHA256 signed string keyed with `ADMIN_TOKEN`, containing an expiry timestamp. `middleware/auth.go` verifies the signature and expiry on every admin request. No DB lookup required.

**On failed validation:** redirect to `/admin/login`, not 403 — to avoid confirming the existence of the admin panel to external scanners.

### token

`crypto/rand` → 32 bytes → base58 encoding → ~43 character string, 256 bits of entropy. Single `Generate() string` function. Lookup-only, no verification logic.

---

## 5. Data flows

### 5a. Outgoing transfer — happy path

```
1. Internal user opens GET /
   → Renders send.html with branding from settings cache

2. User fills form, selects files, clicks Send
   → POST /send
   → Creates: transfer (pending) + files (uploading) + recipients + download tokens
   → Returns JSON { transfer_id } to browser JS
     (one transfer_id only — no per-file tokens)

3. Browser JS initiates one TUS upload per file to POST /tus/
   → Sends X-Transfer-Id: <transfer_id> and X-Filename: <original_name> as metadata
   → TUS server creates the upload resource, returns a Location URL
   → PreUploadCreateCallback: validates transfer exists, pending, not expired
   → tusd writes chunks; each PATCH updates tus_last_activity_at
   → The browser JS tracks which files are uploading using the TUS Location URLs,
     not pre-issued tokens — TUS generates its own upload IDs server-side

4. Each file's final chunk arrives → UploadFinisher fires
   → file.status = 'complete', tus_upload_id = NULL
   → Atomic UPDATE: set transfer active if and only if all files are now complete
   → If this UPDATE affected 1 row (this goroutine won the race):
       INSERT mail_queue: one row per recipient
       INSERT mail_queue: one confirmation to sender

5. Mail job picks up pending rows, sends via SMTP with retry

6. Recipient opens GET /dl/:token
   → Validates token: transfer active, not expired
   → If password set: prompt first
   → Renders download page with file list

7. Recipient clicks a file → GET /dl/:token/file/:file_id
   → Handler explicitly sets Content-Disposition header (RFC 5987, see §7)
     BEFORE calling http.ServeContent — ServeContent does not set this itself
   → http.ServeContent streams the file, handling Range requests automatically
     (HTTP 206 Partial Content for resumed downloads, 200 for full downloads)
   → In one transaction:
       INSERT download_events (with original_name denormalised)
       UPDATE recipients: download_count++, first_download_at if NULL
       INSERT mail_queue: download notification to sender (if enabled)

8. Transfer expires
   → Expiry job: transfer.status = 'expired', expired_at = now()
   → Queries download_events for all recipients (original_name still present
     even after file deletion, because it is stored on the event row)
   → INSERT mail_queue: expiry summary mail
   → Cleanup job (after grace period): os.RemoveAll storage directory,
     files.status = 'deleted', download_events.file_id = NULL
```

### 5b. Upload request — happy path

```
1. Internal user creates upload request → POST /request
   → upload_request (open) + upload_token

2. External party opens GET /ul/:token
   → Validates: open, not expired, optional password
   → Renders upload page with instructions

3. External party uploads → POST /tus/ with X-Upload-Request-Token
   → PreUploadCreateCallback validates token
   → Files land in storage at requests/<upload_request_id>/

4. External party clicks "Done" → POST /ul/:token/complete
   → upload_request.status = 'completed', completed_at = now()
   → INSERT mail_queue: notification to requester

5. Expiry and cleanup follow same pattern as transfers
```

---

## 6. Database

SQLite file: `db.path` in config (default `/data/app.db`).

### Tables summary

| Table | Purpose |
|-------|---------|
| `transfers` | One row per outgoing transfer |
| `files` | One row per file in a transfer |
| `recipients` | One row per recipient email per transfer |
| `download_events` | Append-only audit log — file_id nullable, original_name denormalised |
| `upload_requests` | One row per upload request |
| `upload_request_files` | One row per file received via upload request |
| `mail_queue` | Persistent outbound mail queue with retry |
| `schema_migrations` | Tracks which migration files have been applied |
| `settings` | Key-value runtime configuration |

### Key design decisions in the schema

**`download_events.file_id` is nullable with `ON DELETE SET NULL`**, not `ON DELETE CASCADE`. This preserves the audit trail after files are deleted by the cleanup job. `original_name` is denormalised onto the event row for the same reason — the expiry summary mail must show filenames even after the files themselves are gone.

**`transfers.activated_at` and `transfers.expired_at`** are separate nullable timestamps rather than a single `updated_at`. They answer specific operational questions: when did this transfer go live? when was it expired? A single `updated_at` would lose the earlier transition timestamp as soon as the next one occurs.

**`mail_queue` startup recovery**: on process start, rows stuck in `sending` for more than 10 minutes are reset to `pending`. This handles the case where the process crashed mid-send.

### Key queries

**Atomic transfer activation (race-safe, multi-file):**
```sql
UPDATE transfers
SET status = 'active', activated_at = unixepoch()
WHERE id = ?
  AND status = 'pending'
  AND (SELECT COUNT(*) FROM files
       WHERE transfer_id = ? AND status != 'complete') = 0;
-- Check affected rows: if 1, enqueue mails. If 0, another goroutine won.
```

**Record download (single transaction):**
```sql
INSERT INTO download_events
    (id, recipient_id, file_id, original_name, ip_address, downloaded_at)
VALUES (?, ?, ?, ?, ?, unixepoch());

UPDATE recipients
SET download_count = download_count + 1,
    first_download_at = COALESCE(first_download_at, unixepoch())
WHERE id = ?;

INSERT INTO mail_queue (id, to_address, subject, body_html, body_text)
VALUES (?, ?, ?, ?, ?);
```

**Expiry summary — works even after file deletion:**
```sql
SELECT r.email, r.download_count, de.downloaded_at, de.original_name
FROM recipients r
LEFT JOIN download_events de ON de.recipient_id = r.id
WHERE r.transfer_id = ?
ORDER BY r.email, de.downloaded_at ASC;
-- Note: original_name from download_events, not files — file may be deleted
```

**Mail job — fetch batch:**
```sql
UPDATE mail_queue SET status = 'sending'
WHERE id IN (
    SELECT id FROM mail_queue
    WHERE status = 'pending' AND next_attempt_at <= unixepoch()
    ORDER BY created_at ASC LIMIT 20
);
-- Then SELECT the rows just set to 'sending' and send them.
```

**Startup recovery for stuck sends:**
```sql
UPDATE mail_queue
SET status = 'pending', next_attempt_at = unixepoch()
WHERE status = 'sending'
  AND COALESCE(last_attempt_at, created_at) < (unixepoch() - 600);
-- COALESCE is essential: last_attempt_at is NULL before any attempt completes.
-- Without it, rows crashed on their first attempt are permanently stuck.
```

---

## 7. File storage

All files are written to `storage.path` (default `/data/storage`). The application treats this as an opaque filesystem.

### Directory structure

```
/data/storage/
├── transfers/
│   └── <transfer_id>/
│       ├── <file_id>            # file content
│       └── <file_id>.info       # TUS resumption sidecar (JSON)
└── requests/
    └── <upload_request_id>/
        ├── <file_id>
        └── <file_id>.info
```

IDs on disk match IDs in the database. Any file can be located from its DB record and vice versa.

### Download streaming

**Range request support (resumable downloads):**

For files of 400-600GB, download interruptions are inevitable. The download handler must support HTTP `Range` requests so browsers and download managers can resume a partial download without starting over.

The handler uses Go's `http.ServeContent` function rather than a bare `io.Copy`:

```go
// Content-Disposition MUST be set before http.ServeContent is called.
// http.ServeContent does NOT set this header automatically — it only
// derives Content-Type from the filename extension. Without an explicit
// Content-Disposition header, browsers may display the file inline
// instead of saving it.
w.Header().Set("Content-Disposition", buildContentDisposition(file.OriginalName))

// ActivatedAt is nullable (sql.NullInt64). Use a safe fallback for modTime
// to avoid a nil-pointer panic if the value is somehow unset.
modTime := time.Now()
if transfer.ActivatedAt.Valid {
    modTime = time.Unix(transfer.ActivatedAt.Int64, 0)
}

http.ServeContent(w, r, file.OriginalName, modTime, f)
```

`http.ServeContent` automatically handles:
- `Range: bytes=<start>-<end>` headers — responds with HTTP 206 Partial Content
- `If-Range` and `If-Modified-Since` conditional requests
- Correct `Content-Range` response headers
- Full response (HTTP 200) when no Range header is present

The file argument must implement `io.ReadSeeker`. `os.File` satisfies this. The modification time argument is used for `Last-Modified` and conditional request validation.

**Do not use `io.Copy` for download responses.** It does not support Range requests and forces a full re-download on every interruption. For a 400GB file on a flaky connection, this is unacceptable.

**Content-Disposition and filename encoding:**

Filenames in a post-production context routinely contain non-ASCII characters and special characters (`Séquence finale.mov`, `Rushes — dag 1.mxf`). A bare `filename="<original_name>"` header breaks for these.

The handler sets both the legacy ASCII fallback and the RFC 5987 encoded parameter:

```
Content-Disposition: attachment; filename="fallback.bin"; filename*=UTF-8''S%C3%A9quence%20finale.mov
```

The `filename` parameter is a sanitised ASCII fallback (non-ASCII replaced with `_`). The `filename*` parameter is the full original name, percent-encoded per RFC 5987. Modern browsers use `filename*` when present; older browsers fall back to `filename`. The Go standard library does not generate this automatically — the handler must construct the header explicitly.

### Cleanup

`os.RemoveAll` on the transfer directory. On success: `files.status = 'deleted'`, `download_events.file_id = NULL` (SET NULL, not CASCADE — preserves audit trail). Transfer row kept indefinitely.

---

## 8. Mail

All mail goes through the `mail_queue` table. No synchronous sends.

### Notification types

| Notification | Inserted by | Recipient |
|---|---|---|
| Transfer available | TUS UploadFinisher | Each recipient (one mail per address) |
| Transfer confirmed | TUS UploadFinisher | Sender |
| File downloaded | Download handler | Sender (if enabled) |
| Expiry summary | Expiry job | Sender |
| Upload request fulfilled | Upload complete handler | Requester |

### Expiry summary format

```
Transfer "Rushes week 23" expired on 16 May 2026.

alice@client.com (3 downloads)
  • 11 May 13:14
  • 11 May 14:48
  • 13 May 09:40

bob@client.com
  Never opened.
```

Uses `download_events.original_name` (denormalised) so filenames are correct even after files are deleted from storage.

---

## 9. Configuration

### config.yaml structure

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  base_url: "https://send.example.com"
  trusted_proxies:
    - "172.16.0.0/12"

storage:
  path: "/data/storage"

db:
  path: "/data/app.db"

ip_allowlist:
  - "10.0.0.0/8"
  - "192.168.0.0/16"

smtp:
  host: "mail.smtp2go.com"
  port: 587
  username: "myuser"
  password: ""                            # set via SMTP_PASSWORD env var
  tls: "starttls"                         # starttls | tls | none
  from_address: "transfers@example.com"
  from_name: "File transfer"

admin:
  token: ""                               # set via ADMIN_TOKEN env var
  session_ttl_hours: 8

expiry_options:
  - label: "1 day"
    hours: 24
  - label: "1 week"
    hours: 168
  - label: "2 weeks"
    hours: 336
  - label: "4 weeks"
    hours: 672

limits:
  max_upload_bytes: 644245094400          # 600 GB
  max_files_per_transfer: 50

jobs:
  expiry_interval_minutes: 60
  cleanup_grace_hours: 24
  mail_interval_minutes: 2
  stall_timeout_hours: 48
  cleanup_interval_hours: 6               # how often the cleanup job runs
  mail_retention_days: 90
```

### Environment variable overrides

| Env var | Config key |
|---|---|
| `SMTP_PASSWORD` | `smtp.password` |
| `ADMIN_TOKEN` | `admin.token` |

### Runtime settings (admin UI → settings table)

| Key | Default | Description |
|---|---|---|
| `branding.company_name` | `My Organisation` | Shown in header and mails |
| `branding.logo_url` | *(empty)* | Empty = show text name only |
| `branding.primary_color` | `#000000` | Main UI color |
| `branding.accent_color` | `#f0c800` | Button and highlight color |
| `branding.bg_color` | `#ffffff` | Page background |
| `ui.welcome_message` | *(empty)* | Shown on send form |
| `mail.from_name` | `File transfer` | Display name in sent mails |
| `mail.from_address` | *(empty)* | Must be set before mails work |
| `mail.notify_on_download` | `true` | Notify sender on each download |
| `mail.expiry_summary` | `true` | Send expiry summary on expiry |

---

## 10. Background jobs

All intervals configurable in `config.yaml`. Jobs run in separate goroutines.

### Mail job — every 2 minutes (configurable: `jobs.mail_interval_minutes`)

```
1. UPDATE mail_queue SET status='sending' WHERE status='pending'
      AND next_attempt_at <= now() LIMIT 20
2. For each 'sending' row:
   a. Send via SMTP
   b. Success: status='sent', last_attempt_at=now()
   c. Failure: attempts++, error_message=<smtp error>
      - attempts < max_attempts: status='pending', next_attempt_at=now()+backoff
      - attempts >= max_attempts: status='failed'
3. Prune sent rows older than mail_retention_days
```

Backoff: 2m → 8m → 30m → 2h → failed.

### Expiry job — every 60 minutes (configurable: `jobs.expiry_interval_minutes`)

```
1. SELECT transfers WHERE status='active' AND expires_at < now()
2. For each:
   a. UPDATE status='expired', expired_at=now()
   b. Query download_events (using denormalised original_name)
   c. INSERT mail_queue: expiry summary to sender
3. Same for upload_requests WHERE status='open' AND expires_at < now()
```

### Cleanup job — every 6 hours (configurable: `jobs.cleanup_interval_hours`)

```
1. SELECT transfers WHERE status='expired'
      AND expires_at < (now() - cleanup_grace_hours * 3600)
2. For each:
   a. SELECT SUM(size_bytes) FROM files WHERE transfer_id=? AND status='complete'
      (record bytes to be freed BEFORE deletion — os.RemoveAll returns no size info)
   b. os.RemoveAll(storage/transfers/<transfer_id>/)
   c. On filesystem error: log, skip, retry next run
   d. On success — run in a single transaction, in this order:
      1. UPDATE download_events SET file_id=NULL
            WHERE file_id IN (SELECT id FROM files WHERE transfer_id=?)
         (must run BEFORE files are marked deleted — the subquery still
          returns file IDs correctly at this point, but doing it after
          would still work; the explicit order removes any ambiguity for
          future maintainers)
      2. UPDATE files SET status='deleted' WHERE transfer_id=?
3. Same for upload_requests and upload_request_files
4. Log total transfers cleaned, total bytes freed (accumulated from step a)
   Note: bytes freed is summed from the DB before deletion, not from the
   filesystem. os.RemoveAll does not return byte counts.
```

### Stalled-upload cleanup job — every 6 hours (same interval as cleanup job: `jobs.cleanup_interval_hours`)

Cleans up TUS uploads abandoned mid-way. Runs independently of transfer expiry.

```
1. SELECT id, storage_path, transfer_id, size_bytes
   FROM files
   WHERE status = 'uploading'
     AND COALESCE(tus_last_activity_at, created_at) < (unixepoch() - stall_timeout_hours * 3600)

   COALESCE is required: if the browser closed before the first TUS PATCH,
   tus_last_activity_at is NULL. Without it, "never started" uploads are
   invisible to the cleanup job and accumulate indefinitely on the NFS share.

2. Accumulate total_bytes_freed += size_bytes for each matched row (before deletion)

3. For each stalled file:
   a. os.Remove(storage_path)          -- remove the partial file content
   b. os.Remove(storage_path + ".info") -- remove the TUS resumption sidecar
      Both must be removed. If only the content file is deleted, the .info
      sidecar remains and TUS will believe the upload can be resumed — but
      the file no longer exists. The next resume attempt will fail with a
      confusing error rather than a clean restart.
   c. On filesystem error: log and continue — do not abort the job
   d. UPDATE files SET status='deleted' WHERE id=?
   e. Do NOT change parent transfer status — stays 'pending',
      allowing the user to start a fresh upload if they want

4. Same for upload_request_files (identical logic, same COALESCE)
5. Log count of files removed and total_bytes_freed
```

---

## 11. Security layers

| Layer | Mechanism | What it prevents |
|---|---|---|
| Network | DMZ firewall | Server cannot reach internal network if compromised |
| TLS | Reverse proxy | Eavesdropping in transit |
| IP allowlist | `middleware/ipallow.go` | Externals cannot create transfers |
| TUS token validation | `PreUploadCreateCallback` | Anonymous storage abuse via public TUS endpoint |
| Admin auth | HMAC cookie, `subtle.ConstantTimeCompare` | Timing attacks, session forgery |
| Token entropy | 256-bit random, base58 | Tokens cannot be guessed |
| Password hashing | bcrypt | DB leak does not expose transfer passwords |
| Container hardening | non-root, read-only FS, no capabilities | Minimal impact if process is compromised |
| File isolation | Files outside web root, never executed | Malicious uploads cannot run |
| Proxy headers | HSTS, CSP, X-Frame-Options | Clickjacking, MIME sniffing |

### Explicitly not protected

- **Files at rest:** Not encrypted by this application. Responsibility of the storage system (ZFS encryption, etc.).
- **Admin brute force:** IP allowlist limits exposure. Rate limiting at reverse proxy recommended.
- **Mail in transit beyond the relay:** Outside application control.

---

## 12. Docker layout

### Dockerfile (multi-stage)

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /ferri ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=builder /ferri /ferri
USER 1000:1000
ENTRYPOINT ["/ferri"]
```

`CGO_ENABLED=0` required for `modernc/sqlite`. No C dependency in the final image.

### docker-compose.yml

```yaml
services:
  app:
    # 'ferri:latest' is the locally-built tag used during development.
    # In production, replace with an explicit version tag (e.g. ferri:1.2.0)
    # so that 'docker compose pull' never silently changes what is running.
    image: ferri:latest
    restart: unless-stopped
    user: "1000:1000"
    read_only: true
    tmpfs:
      - /tmp
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - /mnt/nfs/ferri:/data/storage
      - app_db:/data
    environment:
      - CONFIG_PATH=/config/config.yaml
      - SMTP_PASSWORD=${SMTP_PASSWORD}
      - ADMIN_TOKEN=${ADMIN_TOKEN}
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "/ferri", "-health"]
      interval: 30s
      timeout: 5s
      retries: 3

volumes:
  app_db:
```

### Litestream sidecar

```dockerfile
COPY --from=litestream/litestream:0.3.13 /usr/local/bin/litestream /litestream
COPY litestream.yml /etc/litestream.yml
ENTRYPOINT ["/litestream", "replicate", "-exec", "/ferri"]
```

Litestream starts before the application and wraps it. If Litestream exits, the application exits — Docker restarts both. Credentials via `LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY` env vars.

**Litestream compatibility with `read_only: true`:**

The container filesystem is read-only, but Litestream needs writable paths for its own operation. Two paths must be writable:

1. `/data` — the SQLite database and its WAL file. This is provided by the `app_db` named volume.
2. `/tmp` — Litestream uses the system temp directory for staging. This is provided by the `tmpfs` mount.

The combination works, but is sensitive to Litestream version changes. If a future Litestream version writes to a different path (e.g. a cache directory outside `/tmp`), the container will fail at startup with a permission error. Pin the Litestream version in the Dockerfile (`FROM litestream/litestream:0.3.x`) and test after any Litestream upgrade. Do not use `:latest` in production.

---

*Last updated: 2026-05-16. Update this document whenever a component changes meaningfully.*
