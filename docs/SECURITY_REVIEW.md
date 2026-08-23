# SECURITY_REVIEW.md

Log of the project analysis and security review performed on 2026-08-23, plus
the status of every follow-up fix. This file is a living tracker — update the
status table as items are addressed, don't rewrite history.

---

## How this review was done

Two independent full-codebase passes (architecture/quality, and security),
each reading the actual Go source rather than trusting `CLAUDE.md`'s claims.
Findings below were then spot-verified against the code directly.

---

## Status tracker

| # | Finding | Severity | Status |
|---|---|---|---|
| 1 | Password bypass on `/ul/{token}/files`, `/file/{id}`, `/zip` | High | **Fixed** |
| 2 | No CSRF protection on `/send` and `/request` | Medium | **Fixed** |
| 3 | `DeriveKey` for SMB password is bare SHA-256, not a real KDF | Medium | **Fixed — ⚠ needs SMB password re-entry on deploy, see work log** |
| 4 | Download audit trusts `X-Real-IP` unconditionally | Medium | **Fixed** |
| 5 | No rate limiting / lockout on admin login or public tokens | Low | Deliberately not actioned — see work log |

Architecture/quality findings (not security, tracked here for completeness,
not being actioned unless requested):

| # | Finding | Status |
|---|---|---|
| A1 | Logo upload/delete bypasses storage abstraction (always local disk) | Not actioned |
| A2 | Cleanup job: request-fetch error doesn't `return` (scheduler.go) | Not actioned |
| A3 | Expired transfers with zero files never get cleaned up | Not actioned |
| A4 | SMB reconnect logic matches on error substrings, not `errors.Is` | Not actioned |
| A5 | `CLAUDE.md` stale on template usage (admin/mail.html, request.html) | Not actioned |
| A6 | Dead store methods `ListActive`/`ListAll` | Not actioned |
| A7 | Stale "placeholder" comments in shipped code | Not actioned |
| A8 | No test suite at all (`*_test.go` — zero results) | Not actioned |

---

## What's good (architecture)

- Storage abstraction (`internal/storage/backend.go`): `Manager` hot-swaps
  local/SMB backends behind a `RWMutex`; TUS calls re-resolve the backend
  each time instead of caching it.
- `TransferStore.TryActivate` (`internal/store/transfer.go:230-246`) resolves
  a race between concurrently-completing uploads with a single conditional
  `UPDATE`, checked via `RowsAffected()`.
- Graceful shutdown (`cmd/server/main.go:176-197`): signal handling,
  context-based `srv.Shutdown`, scheduler waits on a `WaitGroup`.
- Download streaming uses `http.ServeContent` (Range/206 support), with a
  deliberate multi-layer fallback for locating files on storage.

## What's good (security)

- Tokens (`internal/token/token.go`): `crypto/rand`, 256-bit entropy, base58.
- Admin session cookie: HMAC-SHA256 signed, expiry embedded, `hmac.Equal`
  constant-time check, `HttpOnly`, `SameSite=Lax`, `Secure` configurable.
- Admin token comparison uses `subtle.ConstantTimeCompare`
  (`internal/handler/admin.go:67`).
- All SQL in `internal/store/*.go` is parameterized — no injectable queries,
  no `exec.Command`/shell usage anywhere.
- Path traversal not exploitable: all IDs are server-generated (base58,
  no `/` or `.`) and validated against the DB before use.
- Templates use `html/template` auto-escaping; no `template.HTML()` bypass
  found. SVG is correctly stripped from logo uploads (XSS prevention).
- All five security claims in `CLAUDE.md` were verified true: security
  headers middleware, SVG-strip on logo upload, configurable secure cookies,
  `/admin/orphans/clean` and `/admin/diag` both behind `AdminAuth`+`IPAllow`.

---

## Security findings, in detail

### 1. High — password bypass on upload-request download routes

`GET /ul/{token}/files`, `/ul/{token}/file/{fileID}`, `/ul/{token}/zip`
(`internal/handler/upload.go:251, 283, 322`) call
`stores.Requests.GetByUploadTokenAny`, which does not check
`PasswordHash`/`uploadPasswordValid` at all — unlike the main page handler
(`upload.go:60-65`) and `/ul/{token}/complete` (`upload.go:153-159`), which
both correctly gate on the password.

**Impact:** anyone holding the upload link (meant only to let them submit
files) can view and download all files received for that request, bypassing
the password, whenever a password is set on the request.

**Fix:** add the same `uploadPasswordValid(r, tok, req.PasswordHash.String)`
check (when `req.PasswordHash.Valid`) to `RequestDownloadPage`,
`RequestDownloadFile`, and `RequestDownloadZIP`, returning the password
prompt / 401 instead of serving content when it fails.

### 2. Medium — no CSRF protection on `/send` and `/request`

`cmd/server/main.go:131-137`. These IP-allowlisted routes have no CSRF
token; a malicious external page can cross-site-POST to `/send` from a
victim's browser while it is on the allowed LAN, creating transfers or
triggering mail through the org's SMTP relay. Admin routes are safer because
the session cookie is `SameSite=Lax`, which blocks cross-site POST.

### 3. Medium — SMB password key derivation is bare SHA-256

`internal/storage/encrypt.go:15`. `DeriveKey(ADMIN_TOKEN)` has no iteration
cost (no PBKDF2/scrypt/argon2). Not exploitable while `ADMIN_TOKEN` is
always a full 32-byte random value as documented, but offers no brute-force
margin if that assumption is ever violated.

### 4. Medium — download audit trusts `X-Real-IP` unconditionally

`internal/handler/download.go:202-210, 404-411`. Unlike the `IPAllow`
middleware's trusted-proxy check, this header is trusted as-is and is
spoofable by any client, undermining the download audit trail.

### 5. Low — no rate limiting anywhere

Admin login has only a flat 500ms sleep on failure, no lockout. Public
token endpoints (download/upload links) have no rate limiting at all,
mitigated in practice by the 256-bit token entropy.

---

## Testing gap

`find . -name '*_test.go'` returns nothing — there is no test suite. No unit
tests for the store layer, the cleanup/expiry scheduler, TUS callbacks, or
config validation. Flagged as the single largest structural risk in the
codebase but out of scope for the security fixes below unless requested.

---

## Work log

- **2026-08-23** — Review performed (2 parallel full-codebase passes:
  architecture/quality + security), finding #1 spot-verified directly
  against `internal/handler/upload.go`. This file created. Starting on
  fixes for items #1–#5 above.
- **2026-08-23** — Fixed #1: added the same `uploadPasswordValid` gate used
  by `GET /ul/:token` to `RequestDownloadPage`, `RequestDownloadFile`, and
  `RequestDownloadZIP` in `internal/handler/upload.go`. The page handler
  re-renders the password prompt (`renderUploadPasswordPage`); the two
  binary-response handlers (file, zip) redirect to `/ul/:token` instead,
  mirroring the existing pattern in `internal/handler/download.go:157-163`
  for the equivalent `/dl/:token/*` routes. Verified with `go build ./...`.
- **2026-08-23** — Fixed #2: added `internal/middleware/csrf.go`
  (`CSRFOriginCheck`), an Origin-header verification middleware — no
  session/token infra exists on the `/send` and `/request` route group to
  hang a synchronizer token off, so origin verification is the standard
  mitigation for that shape of route. Wired into the IP-restricted route
  group in `cmd/server/main.go` alongside `IPAllow`. Confirmed both call
  sites (`fetch()` in `static/files/upload.js:130` for `/send`, and the
  plain `<form method="POST" action="/request">` in
  `web/templates/send.html:103`) send an `Origin` header on POST in all
  current browsers, so this doesn't break the existing UI. Verified with
  `go build ./...` and `go vet ./...`.
- **2026-08-23** — Fixed #3: `internal/storage/encrypt.go` `DeriveKey` now
  uses HKDF-SHA256 (`golang.org/x/crypto/hkdf`, already an indirect
  dependency via `x/crypto`) with a fixed domain-separation info string,
  instead of a bare `sha256.Sum256`. Verified with `go build ./...`.
  **Operational impact:** this changes the derived AES key, so any SMB
  password already encrypted-and-stored under the old key will fail to
  decrypt after this change ships — functionally identical to the existing
  documented "ADMIN_TOKEN changed → re-enter SMB password" scenario in
  `CLAUDE.md`. **Action needed on next deploy:** re-enter and save the SMB
  password via `/admin/settings/storage` once this build is running, on any
  environment that has SMB storage configured. Local-storage-only
  deployments are unaffected.
- **2026-08-23** — Fixed #4: exported `clientIP` as `ClientIP` in
  `internal/middleware/ipallow.go` (already implements the correct
  trusted-proxy check used by `IPAllow`) and reused it in
  `internal/handler/download.go` `DownloadFile` and `DownloadZIP`, replacing
  the unconditional `r.Header.Get("X-Real-IP")` read. Both now call
  `appMiddleware.ClientIP(r, cfg.TrustedProxies)`, matching how the rest of
  the app already treats forwarded headers. Removed the now-unused `net`
  import. Verified with `go build ./...` and `go vet ./...`.
- **2026-08-23** — Deliberately did **not** build rate limiting/lockout for
  #5. Reasoning: `ADMIN_TOKEN` and all transfer/upload tokens carry 256 bits
  of entropy (`internal/token/token.go`) — brute-forcing either is
  computationally infeasible regardless of rate limiting, so this is a
  defense-in-depth/detection gap, not an exploitable weakness, which is why
  the original review scored it Low. A real fix means a stateful IP-tracking
  lockout (in-memory or persisted, with a lockout-duration policy) — that's
  a product decision (how long to lock out, per-IP vs. global, whether it
  should survive a restart) worth deciding deliberately rather than bolting
  on silently, and a wrong lockout policy is itself a self-inflicted DoS
  risk against the admin. Left as-is; happy to build it if wanted, once the
  lockout policy is decided.
