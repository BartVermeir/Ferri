# DECISIONS.md

This file records all significant architectural and product decisions made during the design and development of this project. Each entry explains **what** was decided, **why**, and **what alternatives were considered and rejected**.

This document is intended as a living record. When a decision is revisited or reversed, add a new entry rather than editing the old one.

---

## Project context

**Product scope:** Generic, open-source, self-hosted file transfer tool
**Product scope:** Generic, open-source, self-hosted file transfer tool  
**Decision date:** 2026-05-16  
**Status:** Initial architecture decisions

---

## DEC-001: Backend language — Go

**Decision:** The backend is written in Go using the standard library (`net/http`) with the `chi` router.

**Rationale:**
- Compiles to a single static binary with no runtime dependencies. The Docker image can be built on a `scratch` or `distroless` base, minimizing attack surface and image size.
- Excellent I/O performance for large file streaming. Go's concurrency model (goroutines) handles parallel TUS chunk uploads without blocking.
- The `tusd` library (reference TUS implementation) is written in Go, allowing tight integration.
- Low operational complexity for a single-administrator deployment.
- Strong standard library for HTTP, SMTP, templating, and filesystem operations.

**Alternatives considered:**
- **Python/FastAPI:** More familiar to many developers, but slower for large file I/O, and the deployment story (venv, pip, runtime) adds complexity.
- **Node.js:** Event loop works well for I/O-bound tasks, but large binary streaming in Node requires careful stream management and has historically had issues with backpressure on very large files.
- **Rust:** Maximum performance, but development velocity is slower. The performance advantage over Go is not justified for this use case.

---

## DEC-002: Database — SQLite with Litestream

**Decision:** SQLite is used as the sole database. Litestream runs as a sidecar process to continuously stream the SQLite WAL to a backup destination.

**Rationale:**
- This is a single-node, single-administrator deployment. There is no concurrent write workload that would stress SQLite.
- Zero operational overhead: no separate database container, no connection pooling, no user management.
- The metadata volume (transfers, tokens, audit logs) is trivially small compared to the file storage volume. SQLite at 100MB is already large for this workload.
- Litestream provides continuous replication of the SQLite WAL, making point-in-time recovery possible without a separate backup process.
- SQLite files are trivially portable and inspectable with standard tooling.

**Alternatives considered:**
- **PostgreSQL:** Justified if multi-node or high-concurrency is needed. Adds an extra container, connection management, and operational burden. Not warranted for this scale.
- **Redis:** No use case requiring in-memory data structures or pub/sub at this stage.
- **Embedded key-value stores (bbolt, badger):** Less expressive than SQL for queries like "find all transfers expiring before date X". SQL is the right abstraction.

**Upgrade path:** If a future deployment requires multi-node or higher concurrency, the schema can be migrated to PostgreSQL. The repository will include migration tooling for this path.

---

## DEC-003: Upload protocol — TUS (tus.io)

**Decision:** Resumable uploads use the [TUS open protocol](https://tus.io). The server embeds the `tusd` handler. The browser client uses `tus-js-client`.

**Rationale:**
- TUS is an open, well-documented protocol specifically designed for large resumable uploads.
- `tus-js-client` is mature, actively maintained, and works in all modern browsers without plugins or native installations.
- `tusd` is the reference server implementation in Go, making integration with our Go backend straightforward.
- TUS supports pause, resume, and crash recovery via a stable upload ID. This is a hard requirement for 500-600GB uploads.
- The protocol is storage-agnostic: the TUS handler writes to any filesystem path.

**Alternatives considered:**
- **S3 multipart upload:** Requires S3-compatible storage. The target deployment uses NFS/ZFS. Introducing an S3 gateway (MinIO) adds complexity and a moving part without benefit.
- **Custom chunking implementation:** Re-inventing TUS. Not justified when a mature open standard exists.
- **Resumable.js / Flow.js:** Client-only chunking libraries, not a complete protocol. Server-side state management would need to be built from scratch.

---

## DEC-004: Frontend — Vanilla HTML/CSS/JS with Go templates

**Decision:** The frontend is rendered server-side using Go's `html/template` package. Interactivity (upload progress, password reveal, etc.) uses vanilla JavaScript. No frontend build pipeline.

**Rationale:**
- No Node.js, npm, or build step required on the deployment host or in CI. The Docker build compiles the Go binary; templates and static assets are embedded via `go:embed`.
- The application has limited UI surface area: a send form, a download page, an upload request page, and an admin panel. None of these require SPA-level interactivity.
- Branding (colors, logo, company name) is injected via CSS custom properties at runtime, driven by the config.
- `tus-js-client` is the only external JavaScript dependency; it is bundled as a static asset.

**Alternatives considered:**
- **React/Next.js:** Justified for complex, highly interactive UIs. Adds build pipeline complexity and a Node.js requirement. Not warranted here.
- **HTMX:** Interesting for server-driven interactivity, but adds a dependency and learning curve for a future maintainer without meaningful benefit for this UI surface.
- **Vue/Svelte:** Same argument as React. The UI is simple enough that vanilla JS is less complex than any framework.

---

## DEC-005: Storage — local folder or SMB share, behind one interface

**Decision:** Files live either in a local folder or on an SMB share. Both implement one `storage.Backend` interface (open, stat, remove, a tusd data store, free space). A `storage.Manager` holds the active backend and can swap it at runtime: the admin chooses local or SMB under Settings → Storage, without a restart. Uploads are stored flat (`<root>/<tus_upload_id>` plus `.info`) on both.

**Rationale:**
- A local folder covers a local disk and anything the host mounts (NFS, ZFS, a CIFS mount).
- The SMB backend (go-smb2, negotiates up to SMB 3.1.1) talks to a share directly, without a mount on the host. Why this was built into the app on 2026-05-20 rather than left to a host mount was not recorded.
- A swap closes the old backend only after its last open file or running upload call (audit L13).

**Alternatives considered:**
- **S3-compatible object storage:** adds a dependency for on-premises deployments and a TUS S3 integration. Not justified.

**Operational note:** the SMB password is stored encrypted (DEC-041). The storage folder may not contain the database (orphan cleanup would treat it as an unknown file).

---

## DEC-006: IP restriction — Middleware on create endpoints

**Decision:** A middleware layer checks the client IP against a configured list of CIDR ranges. This middleware is applied only to the "create transfer" and "create upload request" endpoints. Download and upload-via-token endpoints are publicly accessible.

**Rationale:**
- Matches the stated security model: link creation is internal-only, link consumption is public.
- Implemented in application code rather than relying solely on a reverse proxy, so the restriction is enforced even if the proxy is misconfigured.
- CIDR ranges are configured in the config file (e.g., `10.0.0.0/8`, `192.168.1.0/24`).
- `X-Forwarded-For` and `X-Real-IP` headers are respected when the application is behind a reverse proxy, with a configurable `TRUSTED_PROXIES` setting.

**Alternatives considered:**
- **Reverse proxy only (nginx/Caddy):** Single point of enforcement. A misconfigured proxy would expose the create endpoints. Defense in depth is preferred.
- **VPN requirement:** Operationally complex for internal users. Not requested.

---

## DEC-007: Mail — Direct SMTP

**Decision:** Mail notifications are sent via direct SMTP using configurable host, port, and credentials.

**Note: this decision was partially revised by DEC-012.** The original rationale excluded external mail services entirely. DEC-012 revised this to explicitly support a hosted SMTP relay service as a valid configuration alongside on-premises relays (Postfix, Exchange). The application uses standard SMTP in both cases — no vendor-specific SDK or API. The "no external service" rationale below applies to services with proprietary APIs (Sendgrid, Mailgun), not to standard SMTP relays.

**Rationale:**
- The deployment is fully on-premises. External mail services (Sendgrid, Mailgun, etc.) introduce an external dependency and potential data leakage.
- SMTP configuration is standard and well-understood by IT administrators.
- `wneessen/go-mail` builds the HTML and plain-text MIME messages and speaks SMTP (it replaced the unmaintained `jordan-wright/email` on 2026-09-22). One connection is reused for a whole batch of the mail job (audit O5).

**Notifications sent:**
1. To recipient(s): transfer available (includes download link)
2. To sender: confirmation that transfer was created
3. To sender: notification when a recipient downloads (at most one per recipient and transfer per hour; see DEC-012)
4. To requester: upload request completed by external party

**Alternatives considered:**
- **External transactional mail services:** Rejected for on-prem requirement.
- **No mail at all:** Mail notification was explicitly required.

---

## DEC-008: Deployment — Docker + Docker Compose, single node

**Decision:** The primary deployment method is Docker Compose on a single host. No Kubernetes or container orchestration is required.

**Rationale:**
- Single-administrator, single-organization deployment. There is no redundancy or horizontal scaling requirement.
- Storage is on an external NFS or SMB share, so data is not at risk if the application container restarts.
- Docker Compose is the simplest operational model that Docker-literate administrators can maintain.
- A single `docker-compose.yml` with clear comments is more maintainable than Helm charts or Kubernetes manifests for this use case.

**Alternatives considered:**
- **Kubernetes:** Overkill for single-node. Significant operational overhead for one administrator.
- **Bare metal / systemd service:** Possible but reduces portability. Docker is the baseline.

---

## DEC-009: Configuration — Two-tier model

**Decision:** Configuration is split into two tiers:

1. **Deploy-time config (config file / environment variables):** Settings that require a container restart to change. Includes: SMTP credentials, storage path, IP ranges, domain name, max file size, expiry options, TLS settings. Format: YAML config file, with environment variable overrides for secrets.

2. **Runtime config (admin UI + database):** Settings changeable without restart. Includes: branding (logo, colors, company name), welcome message, notification templates, maintenance mode.

**Rationale:**
- Separating concerns: infrastructure settings belong in config files (managed by the operator), while branding and messaging belong in the admin UI (managed without server access).
- Environment variable overrides for secrets follow the 12-factor app pattern and integrate well with Docker secrets and CI/CD pipelines.
- Storing runtime config in the database means it persists across container restarts and is included in Litestream backups.

---

## DEC-010: Token design — Opaque random tokens

**Decision:** Download and upload-request tokens are cryptographically random, opaque strings (256 bits of entropy, base58-encoded). Tokens are not JWTs and carry no embedded metadata.

**Rationale:**
- Opaque tokens cannot be forged, decoded, or manipulated by recipients.
- All metadata (expiry, password hash, file list) is stored server-side, looked up by token.
- Revocation is trivial: delete or deactivate the token in the database.
- Base58 encoding avoids ambiguous characters (0, O, I, l) and is URL-safe without percent-encoding.

**URL structure:**
- Download: `/dl/<token>`
- Upload request: `/ul/<upload_token>` for the external party (upload, complete); `/ul/<view_token>/files`, `/file/<id>` and `/zip` for the requester
- Admin: `/admin`

**One token per role:** an upload request has two tokens. The upload link is sent to external parties, and it is often shared with several of them or forwarded. When the same token also opened the received files, everyone holding the upload link could download everything uploaded through it, including other uploaders' files. The requester's view link now uses its own token, and the upload token does not open it. The routes stay the same, and the token decides the role. Requests created before the view token existed keep using their upload token as the view token until they expire, so links in mails already sent keep working.

**Alternatives considered:**
- **JWTs:** Stateless tokens that embed expiry and metadata. Revocation is complex (requires a denylist). Not appropriate when we need server-side revocation control.
- **Sequential IDs:** Enumerable. A security risk for a public-facing token.

---

## DEC-011: Open source strategy

**Decision:** The codebase is published as open source (MIT or Apache 2.0 license, TBD). All configuration values are externalized. No hardcoded references to any specific organisation, infrastructure, or branding exist in the codebase.

**Rationale:**
- The software is generic by design. Any deployment is one instance of a general-purpose tool.
- Hardcoded values would prevent other organizations from adopting the tool without forking.
- Branding, domain names, color schemes, and company names are all runtime configuration.

**What deployment-specific things live outside the repo:**
- `config.yaml` (their SMTP, IP ranges, domain, storage path)
- Docker Compose override file
- Their logo and brand assets (mounted as volumes or configured via admin UI)
- Litestream backup destination credentials

---

---

## DEC-012: Mail — any standard SMTP relay

**Decision:** The SMTP implementation uses standard SMTP (host, port, username, password, TLS). Hosted relay services and on-premises relays (Postfix, Exchange, Office 365 SMTP relay) are supported out of the box.

**Per-recipient mails:** When a transfer is addressed to multiple recipients, each recipient receives a separate mail with their own context. This allows the sender to receive per-recipient download notifications.

**Notification events:**
1. Recipient(s): transfer available (one mail per address)
2. Sender: confirmation of transfer creation
3. Sender: per-recipient download notification, with timestamp. At most one per recipient and transfer per hour, however many files the transfer holds: 5000 files downloaded within the hour give one mail, five transfers give five, and a download the next day mails again. A recipient row belongs to one transfer, so "per recipient" is per recipient per transfer. The mail names the file that triggered it and says further downloads within the hour are not mailed. `RecordDownload` checks for an earlier download inside the transaction that records the new one, so simultaneous downloads cannot both mail. A request that resumes a download (a `Range` not starting at byte 0) is not a new download. Every counted download, repeats included, is still recorded for the expiry summary.
4. Sender: expiry summary — when a transfer expires, or when an admin deletes a live transfer by hand (subject "Deleted: …", no grace period; not for an already expired or a pending transfer), a single summary mail lists every recipient with, per file, the time of each download (not just the first), and the files they did not download. Recipients who downloaded nothing are listed explicitly. A ZIP download, which records every file at the same time, shows as one "All files" line. Example format (architecture.md §8):

   alice@example.org: 2 of 3 files
     • a.mov: 11 May 13:14, 13 May 09:40
     • b.mov: 11 May 13:14
     Not downloaded: c.mov

   bob@example.org: nothing downloaded
5. Upload requester: notification when the external party completes their upload

---

## DEC-013: Server security model — DMZ + container hardening

**Decision:** The application is designed to run in a DMZ (demilitarized zone) network segment, isolated from the internal network by firewall rules. The Docker container runs as a non-root user with a read-only filesystem and all Linux capabilities dropped.

**Threat model:** The primary concern is not data theft from the file store, but lateral movement: a compromised server being used as a pivot point into the internal network.

**Mitigations:**

*Network layer (operator responsibility):*
- Firewall rules: internet → server on port 443 only; server → internal network: nothing (no initiated connections); server → NFS/ZFS: one specific IP and path; server → internet: port 587 (SMTP) only.
- The server has no reason to initiate connections to internal systems. Any such connection attempt is a sign of compromise.

*Container layer (enforced by docker-compose.yml):*
- `user: "1000:1000"` — non-root execution
- `read_only: true` — container filesystem is read-only
- `cap_drop: [ALL]` — no Linux capabilities
- `security_opt: [no-new-privileges:true]` — privilege escalation blocked
- Only the storage volume and /tmp (tmpfs) are writable

*Application layer:*
- Go binary runs as the non-root user
- Uploaded files are stored outside any web-accessible path and are never executed
- File size limits enforced at the reverse proxy level (before reaching the application)
- Content-Type is validated server-side; the browser's declaration is not trusted

*Reverse proxy layer:*
- Caddy (preferred) or nginx sits in front; the Go application only listens on localhost:8080
- TLS termination at the proxy
- Strict headers: HSTS, X-Content-Type-Options, X-Frame-Options, CSP

**What this does NOT protect against:** A vulnerability in the Go application itself that allows reading arbitrary files from the storage mount. Mitigated by the NFS mount being scoped to the transfer storage path only, not a broader internal share.

**Operations note:** The `operations.md` document includes a network configuration checklist that the operator must complete before going live.

---

## DEC-014: Dual file tables — deliberate duplication over unified table

**Decision:** `files` and `upload_request_files` are two separate tables with identical structure rather than one unified table with nullable foreign keys.

**Rationale:**
- A unified table would require a CHECK constraint enforcing that exactly one FK is set. Every query would need to filter on one FK or the other, making all queries more complex.
- The two contexts have different lifecycles and cleanup paths. Mixing them would complicate every job query.
- The TUS handler would need runtime branching on which table to write — duplication in code rather than schema, which is harder to test.
- The duplication is minimal. The cost is that a column added to one must be added to the other. This is mitigated by a comment in the schema (`RULE: if a column is added to files, add it here too`).

**Alternatives considered:**
- **Single `files` table with nullable FKs:** Formally cleaner. Rejected because it complicates all queries and increases risk of cross-context data access.
- **Polymorphic association (owner_type + owner_id):** Cannot be enforced with FK constraints in SQLite — loses referential integrity. Rejected.

---

## DEC-015: Mail delivery — persistent queue with retry, not direct SMTP

**Decision:** Outbound mail is never sent synchronously from request handlers or TUS callbacks. Instead, a row is inserted into the `mail_queue` table. A background job polls every 2 minutes, sends pending mails via SMTP, and retries failures with exponential backoff (max 5 attempts: 2m → 8m → 30m → 2h → failed).

**Rationale:**
- Direct sends from handlers mean a temporary SMTP outage at the moment a transfer becomes active silently drops the recipient notification. For a professional file transfer tool, a lost "your files are ready" mail is a serious operational failure.
- The queue survives container restarts: pending mails are in the database on a persistent volume, not in memory.
- Exponential backoff prevents hammering an SMTP relay that is recovering.
- Failed mails (after max attempts) are visible in the admin dashboard with the SMTP error message stored, so the operator can diagnose the cause.
- Implementation cost is low: one extra table, one extra job, roughly 100 lines of code.

**Alternatives considered:**
- **Direct send in a goroutine:** Zero retry, lost on container restart. Rejected.
- **External queue (Redis, RabbitMQ):** Adds an external dependency. Not justified for the expected mail volume. SQLite as a queue is appropriate at this scale.

---

## DEC-016: Race condition prevention in TUS UploadFinisher — atomic UPDATE pattern

**Decision:** When a TUS upload completes, the transition of a transfer from `pending` to `active` is performed using a single atomic SQL UPDATE that includes a subquery checking whether all sibling files are complete. The affected row count is checked after the UPDATE: if 1, this goroutine activates the transfer and enqueues mails; if 0, another goroutine already did so.

**The problem:** With multiple files uploading concurrently, multiple goroutines can reach the UploadFinisher at nearly the same time. A naive SELECT-then-UPDATE pattern (check if all files complete, then update transfer) has a race window where two goroutines both see an incomplete state and neither activates the transfer.

**The solution:**
```sql
UPDATE transfers
SET status = 'active', activated_at = unixepoch()
WHERE id = ?
  AND status = 'pending'
  AND CASE
        WHEN expected_files IS NOT NULL THEN
          (SELECT COUNT(*) FROM files
           WHERE transfer_id = ? AND status = 'complete') >= expected_files
        ELSE
          (SELECT COUNT(*) FROM files
           WHERE transfer_id = ? AND status != 'complete') = 0
      END;
```
SQLite serialises writes. Only one goroutine can win this UPDATE. The winner checks `RowsAffected() == 1` and proceeds to enqueue mails. All others see 0 and do nothing.

**Why `expected_files`:** the browser uploads files one after another, and a file's row is only created when its upload starts. Checking "no incomplete rows" therefore activated a multi-file transfer as soon as its first file completed, and recipients were mailed a one-file list. `POST /send` now stores how many files the sender announced, and activation waits for that many complete files. It checks "at least", not "exactly": a TUS client that restarts an upload after a 404 leaves the old row `uploading`, and that row must not block the transfer. Transfers created before this column existed have `expected_files = NULL` and keep the old rule.

**Consequences:** an active transfer accepts no new uploads (`ValidateForTUS` requires `pending`). Pending transfers expire like active ones, so an abandoned multi-file upload is still cleaned up. They get no expiry summary, because no link was ever sent.

**Alternatives considered:**
- **Application-level mutex:** Works but requires shared state between goroutines and complicates testing.
- **Separate "check and activate" transaction with BEGIN EXCLUSIVE:** Heavier than needed; SQLite's write serialisation already provides the guarantee we need.

---

## DEC-017: download_events.file_id is nullable with ON DELETE SET NULL

**Decision:** The foreign key from `download_events` to `files` uses `ON DELETE SET NULL`, not `ON DELETE CASCADE`. The `original_name` of the file is denormalised onto the `download_events` row at insert time.

**The problem:** The cleanup job deletes files from disk and marks them `deleted` after a transfer expires. If `download_events` cascaded on file deletion, the entire download history would be wiped before (or simultaneously with) the expiry summary mail being generated — silently destroying the audit trail.

**The solution:** `file_id` becomes NULL when the file is deleted. `original_name` is stored directly on the event row, so the expiry summary query can always reconstruct "alice downloaded Recording_day1.mov on 11 May at 13:14" regardless of whether the file still exists.

**Implication:** The expiry summary query reads `download_events.original_name`, not `files.original_name`. This is documented in the key queries section of architecture.md.

---

## DEC-018: mail_queue startup recovery for stuck 'sending' rows

**Decision:** On application startup, before the job scheduler begins, a SQL UPDATE resets any `mail_queue` rows that are stuck in `sending` status for more than 10 minutes back to `pending`.

**The problem:** The mail job sets a row to `sending` before attempting the SMTP send, to prevent double-sends if the job overlaps itself. If the process crashes after setting `sending` but before completing the send, that row is permanently stuck — never retried, never failed, silently lost.

**The solution:** The 10-minute threshold is conservative: a legitimate SMTP send should never take 10 minutes. Any row still `sending` after 10 minutes is evidence of a crash, not a slow send. The startup hook is in `db/db.go` so it runs before any job or request is handled.

---

## DEC-019: Stalled-upload cleanup is independent of transfer expiry

**Decision:** The stalled-upload cleanup job removes partial TUS uploads after `stall_timeout_hours` of inactivity regardless of the parent transfer's expiry date. The parent transfer is NOT marked expired when stalled files are cleaned up.

**The problem:** The previous design only cleaned up stalled uploads after the transfer expired. A 400GB upload abandoned after 10 minutes would hold NFS storage for up to 4 weeks on a transfer with a 4-week expiry.

**The rationale for not expiring the transfer:** The transfer is still valid — only the upload was abandoned. The internal user who created the transfer may want to start a fresh upload using the same transfer. Expiring the transfer would invalidate their download links and confuse recipients who received the availability notification (though none would have been sent yet, since the transfer never reached `active`).

**What "stalled" means:** `tus_last_activity_at` has not been updated for more than `stall_timeout_hours` (default: 48h). This threshold is intentionally generous to avoid mistaking a legitimately slow upload (e.g. a poor connection uploading a large file) for an abandoned one.

---

## DEC-020: transfers table has activated_at and expired_at, not updated_at

**Decision:** The `transfers` table has two nullable timestamp columns — `activated_at` and `expired_at` — rather than a single `updated_at` column.

**Rationale:** `updated_at` would be overwritten on each status transition, losing earlier timestamps. `activated_at` and `expired_at` answer specific operational questions that come up in practice: when did this transfer go live (for debugging delivery timing)? when was it expired (for verifying cleanup scheduling)? Both timestamps are needed independently and neither should overwrite the other.

**Same pattern applied to `upload_requests`:** `completed_at` was already present; `expired_at` was added in this revision for consistency.

---

## DEC-021: Admin mail queue UI with per-item retry and delete

**Decision:** The admin panel includes a `/admin/mail` page showing pending, failed, and recently sent mails. Individual failed mails can be retried (`POST /admin/mail/:id/retry`, resets attempts to 0) or deleted. The dashboard shows a badge with the count of failed mails.

**Rationale:** A mail queue without operator visibility is a black box. When SMTP fails repeatedly, the operator needs to see why (the error message), decide whether to retry or discard, and have a way to act. Without this, the only recovery path is directly editing the SQLite database — unacceptable for a tool targeting non-developer administrators.

---

## DEC-022: COALESCE in NULL-sensitive cleanup queries

**Decision:** All background job queries that filter on potentially-NULL timestamp columns use `COALESCE(nullable_col, created_at)` rather than a bare comparison.

**Affected queries:**

1. Startup recovery for stuck `mail_queue` rows:
   `COALESCE(last_attempt_at, created_at) < (unixepoch() - 600)`
   `last_attempt_at` is NULL until a send attempt completes or fails. A row that was set to `sending` and then crashed before any attempt finished has `last_attempt_at = NULL`. `NULL < X` evaluates to NULL in SQL, treated as false — the row would be permanently stuck without the COALESCE.

2. Stalled-upload cleanup for `files` and `upload_request_files`:
   `COALESCE(tus_last_activity_at, created_at) < (unixepoch() - stall_timeout)`
   `tus_last_activity_at` is NULL if the browser closed before the first TUS PATCH arrived. These "never started" uploads are invisible to the cleanup job without the COALESCE, and accumulate indefinitely on the NFS share.

**General rule:** Any query that filters `col < threshold` where `col` can be NULL must use `COALESCE(col, sensible_fallback)`. In this codebase, `created_at` is always the correct fallback because it is always set and represents the latest moment at which the row could have been active.

---

## DEC-023: Content-Disposition filename encoding — RFC 5987

**Decision:** The download handler sets both a legacy ASCII `filename` parameter and a `filename*` parameter (RFC 5987, UTF-8 percent-encoded) in the `Content-Disposition` header.

**The problem:** Media filenames routinely contain non-ASCII characters (`Café scene.mov`, `Recording — day 1.mov`, `제작_최종.mov`). A bare `Content-Disposition: attachment; filename="Café scene.mov"` header is not valid per RFC 6266 and breaks in various browsers and download managers.

**The solution:**
```
Content-Disposition: attachment; filename="Sequence_finale.mov"; filename*=UTF-8''S%C3%A9quence%20finale.mov
```

- `filename`: ASCII-only fallback with non-ASCII characters replaced by `_`. Used by old browsers.
- `filename*`: Full original name, UTF-8 percent-encoded per RFC 5987. Used by all modern browsers.

The Go standard library does not generate this automatically. The handler constructs the header explicitly. This must not be simplified to a bare `filename=` at any point — the test case is a filename with an accented character or an em-dash.

---

## DEC-024: Bytes-freed logging requires pre-deletion DB sum, not filesystem measurement

**Decision:** The cleanup job logs bytes freed by summing `size_bytes` from the `files` table *before* calling `os.RemoveAll`, not by measuring filesystem usage before and after.

**The problem:** `os.RemoveAll` returns only an error or nil — no byte count. Post-deletion filesystem measurement is unreliable (NFS caching, filesystem block size rounding, concurrent writes). The only reliable byte count is the one stored in the database.

**Implementation:** For each transfer being cleaned up, the job runs:
```sql
SELECT COALESCE(SUM(size_bytes), 0) FROM files WHERE transfer_id = ? AND status = 'complete'
```
before deletion, accumulates the total across all cleaned transfers, and logs once at the end of the job run.

**Limitation:** `size_bytes` is updated as TUS chunks arrive and reflects the final file size on completion. Partially-uploaded files (stalled uploads) may have a lower `size_bytes` than what is actually on disk. The logged number is therefore a lower bound for stalled-upload cleanup, and exact for completed-transfer cleanup.

---

## DEC-025: Litestream version pinning in Dockerfile

**Decision:** The Litestream image in the Dockerfile is pinned to a specific version tag (e.g. `litestream/litestream:0.3.13`), never `:latest`.

**Rationale:** The container runs with `read_only: true`. Litestream must write only to `/data` (named volume) and `/tmp` (tmpfs). This constraint holds for current Litestream versions but could break silently if a future version writes to a new path (e.g. a cache directory). Using `:latest` means such a regression would appear as a mysterious startup failure after an unrelated `docker compose pull`.

**Upgrade procedure:** Documented in `update-guide.md`. In summary: update the version tag, start the container, verify Litestream is replicating (`docker logs` should show replication activity within 30 seconds), and confirm the application is healthy before considering the upgrade complete.

---

## DEC-026: Download handler uses http.ServeContent, not io.Copy

**Decision:** The file download handler uses Go's `http.ServeContent` rather than a bare `io.Copy` to stream files to the client.

**Rationale:** For files of 400-600GB, download interruptions are a near-certainty on any real network. `io.Copy` streams from byte zero unconditionally — a dropped connection at 95% completion forces a full restart. `http.ServeContent` handles HTTP `Range` requests automatically, responding with HTTP 206 Partial Content so download managers and browsers can resume from the exact byte where they stopped.

`http.ServeContent` requires an `io.ReadSeeker`. `os.File` satisfies this interface directly, so no additional buffering or wrapping is needed.

The modification time argument is set to `time.Unix(transfer.ActivatedAt, 0)` — the moment the transfer became active. This is used for `Last-Modified` headers and conditional request validation (`If-Range`, `If-Modified-Since`).

**Alternatives considered:**
- `io.Copy`: simple but does not support Range requests. Unacceptable for large files.
- Manual Range parsing: reinventing what `http.ServeContent` already does correctly. Rejected.

---

## DEC-027: POST /send returns transfer_id only — no pre-issued per-file tokens

**Decision:** `POST /send` returns a single `transfer_id`. The browser JS uses this as the `X-Transfer-Id` metadata header when initiating each TUS upload. TUS generates its own upload IDs server-side and returns Location URLs to the client.

**Rationale:** TUS is a stateful protocol. The server creates an upload resource on `POST /tus/` and returns a `Location` URL that the client uses for subsequent PATCH requests. There is no concept of a pre-issued per-file token in the TUS protocol — the upload ID is generated by `tusd` at upload-create time, not by the application before the upload starts.

Pre-issuing tokens per file would require a separate round-trip before the TUS upload begins, add state to track which token corresponds to which file, and duplicate the identification mechanism that TUS already provides. The correct flow is: `POST /send` → `transfer_id` → browser sends `POST /tus/` with `X-Transfer-Id` header → tusd creates upload, returns Location → browser PATCHes chunks to Location.

---

## DEC-028: File removal goes through one function, and a failed removal is retried

**Decision:** Every place that deletes an upload's data (expiry cleanup, stalled-upload cleanup, admin delete) calls `storage.PurgeUpload`. It removes all four paths an upload can leave behind: the logical `storage_path` (a real file only for legacy local uploads), the flat TUS file `<tus_upload_id>`, and the `.info` sidecar of each. A missing path counts as removed. Only when all removals succeed is the row's `tus_upload_id` cleared. The row is marked `deleted` either way. Every cleanup run then retries rows that are `deleted` but still have a `tus_upload_id`.

**Rationale:** The data of a TUS upload lives at the flat `<tus_upload_id>`, not at `storage_path`. When each caller removed paths itself, the stalled-upload cleanup removed only `storage_path` and leaked every stalled upload, up to 600 GB each. A single function makes that mistake impossible to repeat.

The `.info` sidecar must go too. With only the content file removed, `tusd` still reports the upload as resumable, and the next resume fails confusingly at the filesystem level instead of cleanly starting a new upload.

**Why the row is marked deleted before the data is confirmed gone:** the download link must stop working immediately, not after a storage retry. A cleared `tus_upload_id` is the separate "physically gone" marker, so no schema change was needed. It also lets the retry find leaks from before this decision, including on SMB, where there is no orphan scan.

**Error handling:** a failed removal is logged and does not abort the run. The file keeps its `tus_upload_id`, is retried on the next run, and stays visible in a WARN log line with count and size until it succeeds.

---

## DEC-029: Upload size limit enforced in PreUploadCreateCallback, not post-upload

**Decision:** The `limits.max_upload_bytes` config value is enforced in the TUS `PreUploadCreateCallback` by checking the `Upload-Length` header sent by the client at upload-create time. Uploads exceeding the limit receive HTTP 413 before any data is written.

**Rationale:** TUS clients send the total intended upload size as an `Upload-Length` header when initiating an upload (`POST /tus/`). This is the only point where size can be checked without consuming storage or bandwidth. Checking size after upload completion would waste NFS space and network capacity, and checking during upload (mid-stream) is complex and unreliable. The `PreUploadCreateCallback` is designed exactly for this kind of pre-flight validation.

**Edge case:** A client that lies about `Upload-Length` (sends a small value but then PATCHes more data) is handled by `tusd` itself — it rejects PATCH requests that would exceed the declared `Upload-Length`. The combination of our check at create-time and tusd's enforcement during transfer closes both attack vectors.

**Gotcha:** the callback must refuse with a `tusd.Error` (`rejectWith` in `internal/tus/handler.go`). For any other error tusd drops the returned response and answers 500, which tus-js-client retries and then reports as "unexpected response". The response body is the plain message, which `upload.js` shows to the user.

---

## DEC-030: transfer.ActivatedAt is sql.NullInt64 — nil-check required before time.Unix

**Decision:** `transfer.ActivatedAt` is mapped to `sql.NullInt64` in Go (not `int64`). All code that converts it to `time.Time` must check `.Valid` before calling `time.Unix`. A fallback of `time.Now()` is used when the value is NULL.

**Rationale:** `activated_at` is NULL in the database until the transfer reaches `active` status. While a download handler should only be reached for active transfers (the token lookup query checks `status = 'active'`), defensive programming requires that the Go code does not assume the value is non-null. A nil-pointer dereference here would produce an HTTP 500 response and a panic log rather than a meaningful error — unacceptable in a handler serving a recipient who is trying to download a file.

**The pattern throughout the codebase:** Any `INTEGER` column that is nullable in the schema must be `sql.NullInt64` (or `sql.NullString`, etc.) in the corresponding Go struct. Non-nullable columns (`NOT NULL` in schema) may be plain Go types. The schema is the source of truth for nullability.

---

## DEC-031: Project name — Ferri

**Decision:** The open source project is named **Ferri**.

**Rationale:** From the Latin *ferre* — to carry, to bring. Short, unique, internationally pronounceable, and not in use by any existing file transfer tool. Available on GitHub. No conflict with existing tooling.

---

## DEC-032: License — MIT

**Decision:** The project is published under the MIT License.

**Rationale:** MIT is the most permissive and most widely adopted open source license for this type of tool. Apache 2.0 adds patent grant clauses that provide little practical benefit for a self-hosted file transfer application. MIT maximises adoptability for other organisations deploying Ferri.

---

## DEC-033: TLS termination — a reverse proxy in front (nginx or Caddy)

**Decision:** The application listens on localhost only and leaves TLS to a reverse proxy on the host. nginx and Caddy are both documented in `operations.md`, including the TUS settings (body size, request buffering, forwarded headers). The reference deployment runs nginx.

**Rationale:** the application stays proxy-agnostic. Caddy provisions Let's Encrypt certificates on its own; nginx fits where a certificate (e.g. a wildcard) already exists. The proxy's address must be in `server.trusted_proxies`, or every visitor appears to come from the proxy.

---

## DEC-034: Virus scanning — not in scope for v1

**Decision:** Virus scanning of uploaded files is explicitly out of scope for the initial release. ClamAV integration may be added as an optional feature in a future version.

**Rationale:** Adding ClamAV introduces a significant operational dependency (ClamAV daemon, signature updates, memory requirements) that is not justified for the initial deployment. The primary threat model is not malicious file content but server compromise and lateral network movement — both addressed by the DMZ architecture and container hardening. If a future deployment requires virus scanning, it can be added as an optional pre-upload hook in the TUS handler without breaking changes to the existing architecture.

---

## DEC-035: Folders are uploaded as loose files, with their structure kept

**Decision:** A folder (selected with "Select a folder" or dropped) is uploaded file by file, like loose files. Each file carries its path inside the folder (`Series/day1/img001.jpg`) as its name. The server cleans that path (`internal/relpath`: no `..`, no absolute or drive parts, no control characters) and stores it. Recipients download single files, or everything as one ZIP that the server builds with the folder structure (`streamZIP`, DEC-038). A transfer or request takes up to `limits.max_files_per_transfer` files (default 5000).

**The problem:** a colleague tried to send a folder, and then its 1600 files. Folders could not be selected, and a dropped folder is not a readable file. The file list was sent as two form fields per file, so 1600 files meant 3202 multipart parts, and Go refuses more than 1000 by default; the server then fell back to an empty form and answered "Sender name is required". The file list is now one JSON field (`files`), and a multipart body that fails to parse is reported as such.

**Why loose files and not one ZIP made in the browser:** every file is its own resumable upload (DEC-037). A file that cannot be read fails alone and is named in the error; a new try continues the same transfer and skips the files that arrived. One ZIP stream of a whole folder could not resume, so any failure restarted everything, and one unreadable file among hundreds failed all of it (seen in a test with 605 files, 102 GB: the browser could not read the first file). The recipient also keeps single-file downloads.

**Limits and risks:**
- Files go one after another; hundreds of small files cost a few requests each.
- Mails and the expiry summary name at most 20 files, with the rest counted; the download pages list every file in a list that scrolls.
- Lists show the path; a single download is named after the file alone.

---

## DEC-036: 600 GB per transfer is a warning; only free space is a hard stop

**Decision:** Ferri checks three things before an upload starts. Only the free-space check is a hard stop that users can meet in normal use.
- **Per upload:** `limits.max_upload_bytes` (600 GB) stays a hard limit per uploaded file (DEC-029).
- **Per transfer or request:** no limit on the total size; up to `max_files_per_transfer` files (5000), folders included. When the files add up to more than 600 GB, the send page and the upload page ask "Upload them anyway?", and on yes the upload goes ahead.
- **Free space:** a new upload is refused (HTTP 507) when it would leave less than `limits.min_free_bytes` (default 50 GB) free on the storage. If the storage cannot report its free space, the upload goes ahead and a WARN is logged.
- **Announced files:** a transfer takes at most twice the number of files `/send` announced. This is not a limit for users, because every new attempt creates a new transfer.

**Rationale:** 600 GB per transfer is what users are told, not a rule the business needs enforced. A 1600-file folder was the first real test, so a limit on the number of files would block exactly the case DEC-035 solves. A full share, by contrast, breaks every upload at once, including other people's, and no warning can prevent that. Letting uploads through when the free space is unknown keeps one SMB server without that query from stopping all uploads.

**Why twice and not exactly the announced number:** when a create response is lost, tus-js-client posts again. The first POST then leaves a dead `uploading` row. With an exact cap, that row would take the place of the transfer's last real file, and the transfer would never go live.

**Considered and rejected (2026-09-25):** limits per upload request on the number of files (200) or total size (1 TB). The per-request columns `max_files` and `max_total_bytes` in `upload_requests` remain unused.

**Limits and risks:**
- The free-space check sees the space at the start of an upload, not what running uploads will still write. The 50 GB margin has to absorb that.
- A 600 GB upload needs 650 GB free, because the upload plus the 50 GB margin must fit.
- On SMB, a unit of free space is go-smb2's `BlockSize()` (bytes per sector) times `FragmentSize()` (sectors per unit). Using `BlockSize()` alone under-reports the space, usually by a factor of 8.

---

## DEC-037: Uploads survive a restart and resume after an error

**Decision:** The browser retries a failed upload request for about 8.5 minutes (`RETRY_DELAYS` in `upload.js`), and every file is resumable: tus-js-client remembers its upload URL in `localStorage` and continues from the server's offset after an error, a new attempt or a reload of the upload page. The remembered key includes the transfer or request (`uploadFingerprint`). A new attempt skips the files that already reached the server: on the upload page, and on the send page too, where it continues the same transfer as long as the form and the file list did not change. `scripts/deploy.sh` warns and asks before restarting when the app handled upload chunks in the last 10 minutes (`FORCE=1` skips the question).

**Rationale:** a deploy stops the app for up to a few minutes. The old retries gave up after 38 seconds, and without resuming a 400 GB upload started over, while a new attempt on the upload page stored every file that had already arrived a second time. Tested in a browser: the app stopped during a 150 MB upload for 20 seconds, and the upload continued and completed with intact bytes.

**Why the transfer or request is in the key:** tus-js-client's own key is name, type, size and date. A second transfer with the same file would then continue the first transfer's upload, and the file would land in the wrong transfer.

**Limits and risks:**
- On the send page, a reload loses the form, so a new attempt creates a new transfer and starts over. The abandoned transfer stays `pending` and expires (DEC-016).

---

## DEC-038: ZIP downloads are stored, only hold complete files, and never fail silently

**Decision:** Transfer and request ZIPs are written by one function (`streamZIP`). Entries are stored, not deflated. Only complete files are included; on the requester's routes, files that are still uploading or broke off are not listed or downloadable either. A file that cannot be opened is left out and named in `MISSING_FILES.txt` inside the ZIP. A failure while a file is being written aborts the response (`http.ErrAbortHandler`), so the browser shows a failed download. ZIP and file names use `buildContentDisposition` everywhere (RFC 5987, accents and spaces intact); an untitled request gives `files.zip`.

**Rationale:** a skipped file used to give a ZIP with status 200 and no word about it, and a read error mid-file gave a truncated file in an archive that looked fine. Deflating video costs a lot of CPU for close to nothing.

---

## DEC-039: "Get a link" — a transfer without mail

**Decision:** Next to "Notify by email", the send form offers "Get a link": no recipients, no mails, the sender gets one shareable download link on the page (`link_only=1`, `transfers.notify_recipients = 0`). The one recipient row carries the sender's address only to hold the link's token. Download notifications for it say "Someone with your shared link", and the expiry summary says "Your shared link".

**Rationale:** for sending a link through another channel (chat, a ticket), without Ferri mailing anyone. Built 2026-06-10.

---

## DEC-040: The sender gets their own link

**Decision:** A transfer with "Notify by email" gets one extra recipient row with `is_sender = 1` (migration 003). Its link goes into the sender's confirmation mail. That row gets no availability mail, triggers no download notification, does not count as a recipient, and is labelled "You (your own link)" in the expiry summary. If the sender is also a recipient, their recipient link is the sender link (unique index on transfer and address). Link-only transfers get no sender link.

**Rationale:** the sender can check or forward the transfer without using, and skewing, a recipient's link. Built 2026-09-25.

---

## DEC-041: SMB password encrypted with a key from Argon2id

**Decision:** The SMB password is stored AES-256-GCM encrypted. The key comes from Argon2id over the admin token, with a fresh random salt per encryption stored with the ciphertext (time 3, 64 MiB, 4 threads). Changing `ADMIN_TOKEN` means entering the SMB password again. A saved password is only reused for the saved host, share, user and domain (audit M4).

**Rationale:** a key derived with a bare hash gave no brute-force margin if the admin token were ever weak; a memory-hard KDF with a salt makes each guess expensive and precomputation useless.

---

## DEC-042: Content Security Policy — scripts only from the app itself

**Decision:** `script-src 'self'`: no inline `<script>`, no inline event handlers, no external script hosts. Page scripts live in `static/files/*.js` (embedded in the binary), data reaches them through `data-*` attributes, and third-party code is vendored (`tus.min.js`).

**Rationale:** injected markup cannot run script even if escaping fails somewhere, and no external host can change the code the page runs. Done 2026-09-23.

---

## DEC-043: Manage link for the sender and the requester, internal network only

**Decision:** Every transfer and upload request gets a `manage_token` (migration 006). `/manage/<token>` shows the status, the files and, for a transfer, per recipient which files were downloaded and when; for a request, the upload and view links. It can extend the expiry (now + one of `expiry_options`, at least an hour later than the current expiry; recipients are not mailed) and delete at once, through the same code as the admin delete (a live transfer mails the sender the "who downloaded what" summary, "deleted by you"). The routes sit behind the IP allowlist, like the send page. Only live items can be managed. Items from before migration 006 have no manage link.

**Rationale:** only the admin could withdraw a transfer sent to the wrong person, or give a request more time. The token is enough to open the page, so it must not work from outside; senders create transfers on the internal network anyway. No login needed.

**Alternatives rejected:** a login (SSO) with a "my transfers" page: much more work and depends on IT. Deleting as "expired" with the grace period: a transfer sent to the wrong person should be gone at once.

---

## DEC-044: "Nothing uploaded yet" reminder goes to the requester

**Decision:** An open upload request that expires within 24 hours without a single complete file, and is at least 24 hours old, gets one reminder mail to the requester, with the upload link to forward again and the manage link to extend. `reminded_at` (migration 007) marks it; extending clears it.

**Rationale:** Ferri does not know the external party's address: the requester sends the upload link by hand. A request made for one day would be reminded right after it was created, so it gets none.

**Alternatives rejected:** an optional "send the link to" field, so Ferri mails the uploader and reminds them: a new invitation mail and form field for a smaller gain.

---

## DEC-045: Admin alerts by mail

**Decision:** Addresses in the admin setting `alerts.recipients` get a mail when the storage cannot be reached or written for 30 minutes, has less than twice `limits.min_free_bytes` free, keeps deleted files for 24 hours, or when mails failed for good. Checked every 15 minutes; at most one mail per kind per 24 hours, state in `alert_state` (migration 008). Failed alert mails are not reported again. Empty = no alerts.

**Rationale:** these problems only showed up as WARN lines in the logs, which nobody reads. The thresholds leave room to act: uploads are refused below `min_free_bytes`, and one failed storage check can be an SMB reconnect.

**Known limit:** if SMTP itself is down, the alerts do not arrive either; the mail queue in the admin panel still shows the failures.

---

*This document is maintained alongside the codebase. All significant decisions must be recorded here before implementation begins.*
