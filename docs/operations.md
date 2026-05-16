# operations.md

This document covers everything needed to install, run, monitor, and troubleshoot this file sharing application in production. It is written for a single administrator who may not be familiar with the codebase but is comfortable with Docker and Linux.

For architectural decisions and component internals, see `architecture.md`. For upgrade procedures, see `update-guide.md`.

---

## Table of contents

1. [Prerequisites](#1-prerequisites)
2. [First-time installation](#2-first-time-installation)
3. [Configuration reference](#3-configuration-reference)
4. [Network configuration checklist](#4-network-configuration-checklist)
5. [Starting and stopping](#5-starting-and-stopping)
6. [Verifying the installation](#6-verifying-the-installation)
7. [Day-to-day administration](#7-day-to-day-administration)
8. [Storage management](#8-storage-management)
9. [Backup and recovery](#9-backup-and-recovery)
10. [Monitoring and alerting](#10-monitoring-and-alerting)
11. [Log reference](#11-log-reference)
12. [Troubleshooting](#12-troubleshooting)
13. [Security checklist](#13-security-checklist)

---

## 1. Prerequisites

### Host machine

- Linux (Ubuntu 22.04 LTS or later recommended)
- Docker Engine 24.0 or later
- Docker Compose v2 (`docker compose`, not `docker-compose`)
- NFS client tools if using NFS storage: `apt install nfs-common`
- Minimum 4GB RAM, 4 CPU cores
- Local disk for OS + Docker volumes: 20GB sufficient
- Network: the host must be in the DMZ — isolated from the internal network by firewall rules (see §4)

### Storage

The NFS share (or other storage backend) must be mounted on the host before starting the container. The application writes all uploaded files there. Ensure:

- The mount point exists: `mkdir -p /mnt/nfs/ferri`
- The NFS share is mounted and writable by the container user (UID 1000)
- The mount is persistent across reboots (entry in `/etc/fstab` or a systemd mount unit)

### DNS and TLS

- A DNS record pointing your chosen hostname (e.g. `send.example.com`) to the host's IP
- A TLS certificate for that hostname (Caddy can obtain one automatically via Let's Encrypt, or supply your own)

---

## 2. First-time installation

### Step 1 — Clone the repository

```bash
git clone https://github.com/your-org/ferri.git
cd ferri
```

### Step 2 — Create the environment file

```bash
cp .env.example .env
```

Edit `.env` and set the required secrets:

```bash
SMTP_PASSWORD=your_smtp2go_password_here
ADMIN_TOKEN=a_long_random_string_at_least_32_chars
LITESTREAM_ACCESS_KEY_ID=your_s3_key_id        # omit if not using S3 backup
LITESTREAM_SECRET_ACCESS_KEY=your_s3_secret    # omit if not using S3 backup
```

Generate a strong `ADMIN_TOKEN`:
```bash
openssl rand -base64 32
```

**Never commit `.env` to version control.** It is in `.gitignore` by default.

### Step 3 — Create config.yaml

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`. The minimum required changes:

```yaml
server:
  base_url: "https://send.example.com"   # your actual domain

smtp:
  host: "mail.smtp2go.com"
  port: 587
  username: "your_smtp2go_username"
  # password comes from SMTP_PASSWORD env var — leave blank here

ip_allowlist:
  - "10.0.0.0/8"          # adjust to your actual internal network range
  - "192.168.1.0/24"      # add as many CIDR ranges as needed
```

See §3 for the full configuration reference.

### Step 4 — Mount the NFS storage

```bash
# Test the mount manually first
sudo mount -t nfs 10.0.1.50:/exports/ferri /mnt/nfs/ferri

# Verify it is writable by UID 1000 — the user the container runs as.
# Testing as root or your own user may succeed even when UID 1000 cannot write.
sudo -u "#1000" touch /mnt/nfs/ferri/.write_test && \
sudo -u "#1000" rm /mnt/nfs/ferri/.write_test && \
echo "NFS writable by UID 1000: OK"
```

Add to `/etc/fstab` for persistence:
```
10.0.1.50:/exports/ferri  /mnt/nfs/ferri  nfs  defaults,_netdev,nofail,rsize=1048576,wsize=1048576  0  0
```

The `rsize` and `wsize` options set the NFS read/write block size to 1MB, which significantly improves throughput for large files. The `nofail` option is important for production: without it, the host will hang at boot if the NFS server is unreachable. With `nofail`, the host boots normally, the NFS mount is skipped, and Docker will simply not start the container (because the storage volume is unavailable) — a recoverable situation. Adjust the IP and export path to match your Dell PowerScale configuration.

### Step 5 — Configure Litestream (optional but recommended)

Litestream continuously backs up the SQLite database. If your deployment has an S3-compatible storage (MinIO, Wasabi, AWS S3):

```bash
cp litestream.example.yml litestream.yml
```

Edit `litestream.yml`:
```yaml
dbs:
  - path: /data/app.db
    replicas:
      - type: s3
        bucket: your-backup-bucket
        path: ferri/db
        region: eu-west-1
        access-key-id: ${LITESTREAM_ACCESS_KEY_ID}
        secret-access-key: ${LITESTREAM_SECRET_ACCESS_KEY}
```

If you are not using S3, configure a local file replica instead:
```yaml
dbs:
  - path: /data/app.db
    replicas:
      - type: file
        path: /backup/ferri-db
```

Mount `/backup` in `docker-compose.yml` as a volume pointing to a separate disk.

### Step 6 — Configure the reverse proxy

**Using Caddy (recommended):**

Caddy is not in Ubuntu's default apt repository. Add the official Caddy repository first:

```bash
# Add the Caddy apt repository (required — Caddy is not in Ubuntu's default repos)
apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list
apt update
apt install caddy
```

`/etc/caddy/Caddyfile`:
```
send.example.com {
    reverse_proxy localhost:8080

    # Large file uploads — disable Caddy's request body buffering
    request_body {
        max_size 650GB
    }

    # TUS requires CORS headers on ALL responses, including OPTIONS preflight.
    # The global header{} block applies to every response Caddy sends,
    # including those generated by respond directives below.
    # These headers must appear before the respond @options directive.
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
        Referrer-Policy "strict-origin-when-cross-origin"

        # CORS headers required for TUS browser uploads.
        # Browsers send OPTIONS preflight before PATCH and before custom headers
        # (X-Transfer-Id, Tus-Resumable, Upload-Metadata).
        # A 204 preflight response without these headers is rejected by the browser.
        Access-Control-Allow-Origin "*"
        Access-Control-Allow-Methods "GET, POST, HEAD, PATCH, OPTIONS, DELETE"
        Access-Control-Allow-Headers "Authorization, Content-Type, Upload-Length, Upload-Offset, Tus-Resumable, Upload-Metadata, Upload-Defer-Length, Upload-Concat, X-Transfer-Id, X-Upload-Request-Token"
        Access-Control-Expose-Headers "Upload-Offset, Location, Upload-Length, Tus-Version, Tus-Resumable, Tus-Max-Size, Tus-Extension, Upload-Metadata"
    }

    # Handle OPTIONS preflight immediately — after headers are set above.
    # Caddy applies the header{} middleware to this response as well.
    @options method OPTIONS
    respond @options 204
}
```

```bash
systemctl enable caddy
systemctl start caddy
```

**Important:** The `max_size` directive must be set to at least your `limits.max_upload_bytes` value. Without it, Caddy will reject large uploads before they reach the application. For 600GB uploads, set it to 650GB or higher.

**Using nginx:**

```nginx
server {
    listen 443 ssl;
    server_name send.example.com;

    ssl_certificate     /etc/ssl/certs/send.example.com.crt;
    ssl_certificate_key /etc/ssl/private/send.example.com.key;

    # Required for large TUS uploads — disable body size limit and buffering.
    # client_max_body_size 0 = no limit. Without this nginx rejects large uploads.
    # proxy_request_buffering off = stream directly to app, do not buffer to disk.
    # Without this, nginx buffers the entire upload before forwarding, which
    # breaks TUS resumability and progress reporting.
    client_max_body_size 0;
    proxy_request_buffering off;

    # Required for TUS protocol (chunked transfer encoding)
    proxy_http_version 1.1;

    location / {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # TUS uploads run for hours on large files — disable proxy timeouts entirely.
        proxy_read_timeout 0;
        proxy_send_timeout 0;

        # TUS CORS headers — required for browser-based uploads.
        # Browsers send an OPTIONS preflight before PATCH requests and before
        # custom headers (X-Transfer-Id, Tus-Resumable, Upload-Metadata).
        # Without these, TUS uploads fail with a CORS error in the browser console.
        add_header Access-Control-Allow-Origin "*" always;
        add_header Access-Control-Allow-Methods "GET, POST, HEAD, PATCH, OPTIONS, DELETE" always;
        add_header Access-Control-Allow-Headers "Authorization, Content-Type, Upload-Length, Upload-Offset, Tus-Resumable, Upload-Metadata, Upload-Defer-Length, Upload-Concat, X-Transfer-Id, X-Upload-Request-Token" always;
        add_header Access-Control-Expose-Headers "Upload-Offset, Location, Upload-Length, Tus-Version, Tus-Resumable, Tus-Max-Size, Tus-Extension, Upload-Metadata" always;

        # Handle OPTIONS preflight immediately without proxying to the app
        if ($request_method = OPTIONS) {
            return 204;
        }
    }
}
```

### Step 7 — Build and start

```bash
# Build the application image
docker build -t ferri:1.0.0 .

# Update docker-compose.yml to use the versioned tag
# image: ferri:1.0.0

# Start everything
docker compose up -d

# Watch the startup logs
docker compose logs -f
```

Normal startup output:
```
app  | litestream: replication started for /data/app.db
app  | running migrations...
app  | migrations complete (1 applied)
app  | startup: resetting 0 stuck mail_queue rows
app  | server listening on 0.0.0.0:8080
```

### Step 8 — Configure branding via admin UI

1. Open `https://send.example.com/admin/login` from your internal network
2. Enter the `ADMIN_TOKEN` value from your `.env` file
3. Navigate to Settings
4. Set: company name, logo URL, colours, from-address for mail
5. Send a test transfer to verify mail delivery

---

## 3. Configuration reference

All deploy-time settings live in `config.yaml`. Sensitive values should be provided via environment variables (see `.env.example`).

| Key | Default | Description |
|---|---|---|
| `server.host` | `0.0.0.0` | Interface to listen on. Keep as-is. |
| `server.port` | `8080` | Port the Go binary listens on. Reverse proxy connects here. |
| `server.base_url` | *(required)* | Full public URL, used in mail links. No trailing slash. |
| `server.trusted_proxies` | `[]` | CIDR ranges of reverse proxies. Used to trust `X-Forwarded-For`. |
| `storage.path` | `/data/storage` | Path inside the container where files are stored. |
| `db.path` | `/data/app.db` | Path to the SQLite database file inside the container. |
| `ip_allowlist` | `[]` | CIDR ranges allowed to create transfers. Empty = nobody can create. |
| `smtp.host` | *(required)* | SMTP relay hostname. |
| `smtp.port` | `587` | SMTP port. 587 = STARTTLS, 465 = TLS, 25 = plain. |
| `smtp.username` | *(required)* | SMTP authentication username. |
| `smtp.password` | *(use env var)* | Set via `SMTP_PASSWORD`. Never in config file. |
| `smtp.tls` | `starttls` | `starttls`, `tls`, or `none`. |
| `smtp.from_address` | *(required)* | Sender address for all outgoing mail. |
| `smtp.from_name` | `File transfer` | Display name for outgoing mail. |
| `admin.token` | *(use env var)* | Set via `ADMIN_TOKEN`. Min 32 characters. |
| `admin.session_ttl_hours` | `8` | Admin session cookie lifetime in hours. |
| `expiry_options` | see example | List of expiry choices shown in the send form. |
| `limits.max_upload_bytes` | `644245094400` | Maximum size per individual file upload (600 GB). Enforced per TUS upload, not per transfer total. A transfer with three 300 GB files is allowed; a single file over 600 GB is rejected at upload-create time with HTTP 413. |
| `limits.max_files_per_transfer` | `50` | Max files per single transfer. |
| `jobs.expiry_interval_minutes` | `60` | How often the expiry job runs. |
| `jobs.cleanup_grace_hours` | `24` | Hours after expiry before files are deleted from disk. |
| `jobs.mail_interval_minutes` | `2` | How often the mail queue is processed. |
| `jobs.stall_timeout_hours` | `48` | Hours of inactivity before an incomplete upload is considered abandoned. |
| `jobs.cleanup_interval_hours` | `6` | How often the cleanup and stalled-upload jobs run. |
| `jobs.mail_retention_days` | `90` | Sent mails are pruned from the queue after this many days. |

**Runtime settings** (changeable without restart, via admin UI):

| Key | Default | Description |
|---|---|---|
| `branding.company_name` | `My Organisation` | Shown in header and mail subject lines. |
| `branding.logo_url` | *(empty)* | URL or path to logo image. Empty = text name only. |
| `branding.primary_color` | `#000000` | Main UI colour (hex). |
| `branding.accent_color` | `#f0c800` | Button and highlight colour (hex). |
| `branding.bg_color` | `#ffffff` | Page background colour (hex). |
| `ui.welcome_message` | *(empty)* | Message shown on the send form. |
| `ui.send_page_title` | `Send files` | Title shown on the send page. |
| `ui.download_page_title` | `Download files` | Title shown on the download page. |
| `mail.from_name` | `File transfer` | Display name in outgoing mails. |
| `mail.from_address` | *(empty)* | **Must be set** before any mail will be sent. |
| `mail.notify_on_download` | `true` | Notify sender each time a recipient downloads. |
| `mail.expiry_summary` | `true` | Send download summary when a transfer expires. |

---

## 4. Network configuration checklist

This checklist must be completed by whoever manages the network/firewall before the system goes live. The application cannot enforce these rules itself — they must be configured on your firewall or switch.

The server should be in a DMZ segment, isolated from both the internet and the internal network.

### Inbound rules (to the server)

| Source | Destination port | Protocol | Purpose |
|---|---|---|---|
| Any (internet) | 443 | TCP | HTTPS — download and upload links |
| Internal network | 443 | TCP | HTTPS — creating transfers, admin UI |
| Internal network | 22 | TCP | SSH — administration only |

### Outbound rules (from the server)

| Destination | Port | Protocol | Purpose |
|---|---|---|---|
| Dell PowerScale IP | 2049 | TCP/UDP | NFS — file storage |
| `mail.smtp2go.com` | 587 | TCP | SMTP — outbound mail |
| S3 backup endpoint | 443 | TCP | Litestream DB backup (if configured) |
| **Internal network** | **ANY** | **ANY** | **BLOCKED — server must not initiate connections into internal network** |
| Internet (other) | ANY | ANY | BLOCKED |

**The most critical rule is the last one:** the server must not be able to initiate connections to internal systems. If the server is compromised, this prevents lateral movement into the rest of the network.

### Verify the firewall rules

After configuring, test from the server itself:

```bash
# This should FAIL — server cannot reach internal systems
curl --connect-timeout 5 http://10.0.0.1/
# Expected: connection timeout or refused

# This should SUCCEED — NFS to storage
showmount -e 10.0.1.50
# Expected: export list shown

# This should SUCCEED — outbound SMTP
nc -zv mail.smtp2go.com 587
# Expected: Connection succeeded
```

---

## 5. Starting and stopping

### Normal start

```bash
cd /opt/ferri   # or wherever you cloned the repo
docker compose up -d
```

### Graceful stop

```bash
docker compose stop
```

This sends SIGTERM to the container. The application catches it, stops accepting new requests, and waits for in-progress file streams to finish before exiting. Docker's default grace period is 30 seconds — after which Docker sends SIGKILL regardless.

For a deployment serving large file downloads, 30 seconds may not be enough. A recipient downloading a 400GB file over a slow connection will have their download interrupted. Set a longer grace period in `docker-compose.yml`:

```yaml
services:
  app:
    stop_grace_period: 300s   # 5 minutes — adjust to your needs
```

Do not use `docker compose kill` for normal stops — it sends SIGKILL immediately with no grace period, interrupting all active uploads and downloads.

### Restart after a change

**After changing `config.yaml`** (SMTP settings, IP allowlist, expiry options, etc.):
```bash
docker compose restart
```
`config.yaml` is mounted as a read-only volume — the application re-reads it on startup, so a restart is sufficient.

**After changing `docker-compose.yml`** (volumes, environment variables, image tag, `stop_grace_period`, etc.):
```bash
docker compose down
docker compose up -d
```
`docker compose restart` does not re-apply changes to the compose file itself. Always use `down` + `up` when the compose file changes.

### Check status

```bash
docker compose ps
docker compose logs --tail=50
```

### Emergency stop

```bash
docker compose down
```

This stops and removes the containers. Named volumes (`app_db`) and the NFS mount are unaffected — no data is lost.

---

## 6. Verifying the installation

Run these checks after first install and after every update.

### 1. Container is running and healthy

```bash
docker compose ps
# Expected: app   running (healthy)
```

If status is `unhealthy`, check logs:
```bash
docker compose logs --tail=100
```

### 2. Application responds

```bash
curl -sv https://send.example.com/health
# Expected: HTTP 200, body: {"status":"ok"}
# This also confirms TLS is working and the reverse proxy is forwarding correctly.
```

### 3. Internal access works

From a machine on the internal network:
```bash
curl -s https://send.example.com/
# Expected: the send form HTML
```

### 4. External access is restricted

From a machine outside the internal network (or use a VPN and disconnect):
```bash
curl -s -o /dev/null -w "%{http_code}" https://send.example.com/
# Expected: 403
```

The download URL should still be accessible externally — only the create endpoints return 403.

### 5. Mail delivery works

1. Log in to the admin UI: `https://send.example.com/admin/login`
2. Check the admin dashboard — "Failed mails" badge should show 0
3. Create a test transfer, send to your own email address
4. Verify you receive the "files ready" notification mail
5. Download the test file and verify you receive the download notification

### 6. Litestream is replicating

```bash
docker compose logs | grep litestream
# Expected: periodic replication log entries, no errors
```

### 7. NFS storage is writable

```bash
# The distroless container has no shell — test from the host instead.
# Test as UID 1000 specifically — this is the user the container runs as.
# A test as root or your own user may succeed even when UID 1000 cannot write.
sudo -u "#1000" touch /mnt/nfs/ferri/.write_test && \
sudo -u "#1000" rm /mnt/nfs/ferri/.write_test && \
echo "NFS writable by UID 1000: OK"
# Expected: NFS writable by UID 1000: OK
# If this fails, adjust NFS export permissions — see §12 for details.
```

---

## 7. Day-to-day administration

### Admin UI

Access at `https://send.example.com/admin` from the internal network.

**Dashboard shows:**
- Active transfers (count, total size, sender)
- Pending upload requests
- Failed mail count (badge — investigate immediately if non-zero)
- Recent expiry job run time

**Common tasks:**

*Delete a transfer early:*
Admin → Transfers → find the transfer → Delete. This soft-deletes the transfer (status = `deleted`) and the cleanup job removes the files on its next run (within `cleanup_interval_hours`, default 6 hours). The sender and recipients receive no automatic notification — contact them manually if needed.

*Retry a failed mail:*
Admin → Mail → find the row → Retry. The mail re-enters the queue with `attempts = 0` and will be attempted within 2 minutes.

*Change branding:*
Admin → Settings → update colours/logo/company name → Save. Changes take effect immediately (settings cache refreshes within 60 seconds).

### Inspecting the database directly

The SQLite database is at `/data/app.db` inside the container, which maps to the `app_db` Docker volume. To inspect it:

```bash
# Install sqlite3 on the host if needed
apt install sqlite3

# Find the volume path
docker volume inspect filetransfer_app_db | grep Mountpoint
# e.g. /var/lib/docker/volumes/filetransfer_app_db/_data

# Query directly (read-only is safe while the app is running in WAL mode)
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT id, sender_email, status, expires_at, datetime(expires_at,'unixepoch') FROM transfers ORDER BY created_at DESC LIMIT 20;"
```

**Write queries while the app is running:** Single atomic statements and explicit `BEGIN/COMMIT` transactions are safe in WAL mode — SQLite serialises writers automatically. What is dangerous is running multiple related statements *without* a transaction: if the process is interrupted between statements, the database is left in a partially-updated state. The rule is: **always wrap multi-statement writes in `BEGIN/COMMIT`**, and avoid touching rows that an active request handler may also be writing to at the same moment. When in doubt, stop the container first with `docker compose stop`.

### Useful queries

**Active transfers and their sizes:**
```sql
SELECT t.id, t.sender_email, t.title,
       COUNT(f.id) AS files,
       ROUND(SUM(f.size_bytes) / 1073741824.0, 2) AS size_gb,
       datetime(t.expires_at, 'unixepoch') AS expires
FROM transfers t
JOIN files f ON f.transfer_id = t.id
WHERE t.status = 'active'
GROUP BY t.id
ORDER BY t.created_at DESC;
```

**Transfers expiring in the next 24 hours:**
```sql
SELECT id, sender_email, title,
       datetime(expires_at, 'unixepoch') AS expires_at
FROM transfers
WHERE status = 'active'
  AND expires_at BETWEEN unixepoch() AND (unixepoch() + 86400)
ORDER BY expires_at;
```

**Failed mails with error messages:**
```sql
SELECT id, to_address, subject, attempts, error_message,
       datetime(created_at, 'unixepoch') AS created
FROM mail_queue
WHERE status = 'failed'
ORDER BY created_at DESC;
```

**Storage usage by transfer:**
```sql
SELECT t.sender_email, t.title,
       ROUND(SUM(f.size_bytes) / 1073741824.0, 2) AS size_gb,
       t.status,
       datetime(t.expires_at, 'unixepoch') AS expires
FROM transfers t
JOIN files f ON f.transfer_id = t.id AND f.status = 'complete'
GROUP BY t.id
ORDER BY size_gb DESC
LIMIT 20;
```

---

## 8. Storage management

### Monitoring storage usage

The NFS share holds all uploaded files. Check usage regularly:

```bash
# On the host — overall usage
df -h /mnt/nfs/ferri

# Breakdown by transfer directory.
# Use find rather than glob (*) to avoid "Argument list too long" on
# deployments with many transfers.
find /mnt/nfs/ferri/transfers -maxdepth 1 -mindepth 1 -type d \
    -exec du -sh {} + 2>/dev/null | sort -rh | head -20
# Note: using '+' instead of '\;' bundles all directories into one du call,
# which is significantly faster over NFS than spawning one process per directory.

find /mnt/nfs/ferri/requests -maxdepth 1 -mindepth 1 -type d \
    -exec du -sh {} + 2>/dev/null | sort -rh | head -10
```

### How files are cleaned up automatically

The cleanup job runs every `cleanup_interval_hours` (default 6 hours). It deletes files for expired transfers that passed the grace period (`cleanup_grace_hours`, default 24h). So the maximum storage retention is:

```
max retention = transfer expiry (up to 4 weeks) + cleanup_grace_hours + cleanup_interval_hours
```

With default values: 4 weeks + 24h grace + 6h interval = **4 weeks and 30 hours** maximum.
The `cleanup_interval_hours` component accounts for the worst case where the cleanup job
just ran before the grace period expired and must wait for the next scheduled run.

### Freeing storage manually

If you need to reclaim space immediately (e.g. a very large transfer that is no longer needed), follow these steps **in this exact order**. The order matters: update the database first, then delete from the filesystem. If the filesystem delete fails partway, the database is already consistent and the cleanup job handles the remainder. If you delete files first and the database update then fails, the transfer is in an inconsistent state and the cleanup job will produce filesystem errors on every subsequent run.

```bash
# Step 1 — Soft-delete via admin UI
# Admin → Transfers → Delete
# This sets transfer.status = 'deleted', stopping new download attempts immediately.
# Wait a moment for any in-progress downloads to finish (check: docker compose logs --tail=20).

# Step 2 — Update the database (wrap in transaction for safety)
TRANSFER_ID="abc123..."   # from admin UI or the queries in §7

sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
"BEGIN;
UPDATE download_events
   SET file_id = NULL
 WHERE file_id IN (SELECT id FROM files WHERE transfer_id = '${TRANSFER_ID}');
UPDATE files
   SET status = 'deleted'
 WHERE transfer_id = '${TRANSFER_ID}';
COMMIT;"

# Step 3 — Delete files from NFS (only after DB is updated)
rm -rf /mnt/nfs/ferri/transfers/${TRANSFER_ID}/
echo "Done."
ls /mnt/nfs/ferri/transfers/${TRANSFER_ID}/ 2>&1   # should show: No such file or directory
```

**Note on multi-statement writes while the app is running:** The statements above are wrapped in `BEGIN/COMMIT` so they execute as a single atomic transaction. This is safe in WAL mode. The admin soft-delete in step 1 ensures no handler is actively serving files from this transfer at the time of the write.

### Orphaned files

Occasionally a directory may exist on storage without a corresponding database entry (e.g. after a crashed cleanup). Find and investigate them:

```bash
# List all transfer directories on NFS
ls /mnt/nfs/ferri/transfers/ > /tmp/on_disk.txt

# List all transfer IDs in the database
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT id FROM transfers;" > /tmp/in_db.txt

# Show directories on disk that are not in the database
comm -23 <(sort /tmp/on_disk.txt) <(sort /tmp/in_db.txt)
```

Investigate any results before deleting — they may be from very recent uploads that have not yet been committed to the database.

### Stalled upload remnants

Partial files from abandoned uploads are cleaned up by the stalled-upload job (runs every `cleanup_interval_hours`, default 6 hours, after `stall_timeout_hours` of inactivity). If you see `.info` files without corresponding content files on the NFS share, they are safe to delete manually — they are TUS resumption sidecars for uploads that no longer exist.

---

## 9. Backup and recovery

### What needs to be backed up

| Data | Location | Backup method |
|---|---|---|
| **Database** | `app_db` Docker volume | Litestream continuous replication |
| **Uploaded files** | NFS share | NFS server's own backup (Dell PowerScale snapshots) |
| **Config** | `config.yaml` + `.env` | Store in a private git repo or password manager |
| **Runtime settings** | Inside the database | Covered by database backup |

The uploaded files are the responsibility of the NFS server's backup system. The application treats the NFS share as opaque storage — it does not back up the files itself.

### Verifying Litestream is working

```bash
# Check for recent replication activity
docker compose logs | grep -i litestream | tail -20
```

Healthy Litestream output looks like this — you should see periodic sync entries,
not just the startup line:
```
app  | litestream: replication started for /data/app.db
app  | litestream: snapshot written to replica            <- appears every ~60s
app  | litestream: wal segment written to replica         <- appears on every DB write
```

If you only see the startup line and nothing after, replication may have stalled.
If you see `level=ERROR` entries, investigate immediately.

```bash
# For S3 replicas: verify files are actually arriving at the backup destination.
# The most recent file should be within the last few minutes.
aws s3 ls s3://your-backup-bucket/ferri/db/ --recursive     | sort | tail -5
# Expected: files with timestamps from the last few minutes.
# If the most recent file is hours old, Litestream has stopped replicating.
```

The database is the only source of truth for all metadata — losing it means losing all transfer records, tokens, and download history. Verify replication weekly at minimum.

### Restoring the database from Litestream backup

**Scenario:** The host fails and you need to restore on a new host.

```bash
# On the new host, after installing Docker and cloning the repo:

# Stop the application if it is running
docker compose down

# Restore the database from S3 backup.
# Note: if the named volume 'filetransfer_app_db' does not exist yet on this host,
# Docker creates it automatically when the 'docker run' command below executes.
# This is correct behaviour — the volume starts empty and Litestream fills it.
docker run --rm \
  -v filetransfer_app_db:/data \
  -e LITESTREAM_ACCESS_KEY_ID=${LITESTREAM_ACCESS_KEY_ID} \
  -e LITESTREAM_SECRET_ACCESS_KEY=${LITESTREAM_SECRET_ACCESS_KEY} \
  litestream/litestream:0.3.13 \
  restore -o /data/app.db \
  s3://your-backup-bucket/ferri/db

# Verify the database is intact
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT COUNT(*) FROM transfers; SELECT COUNT(*) FROM files;"

# Start the application
docker compose up -d
```

**Note:** The uploaded files on the NFS share are separate. After restoring the database, the application will be able to serve downloads again — as long as the NFS share is also mounted and the files are intact.

### Manual database backup (without Litestream)

If Litestream is not configured, back up the database manually:

```bash
# Create the backup directory if it does not exist.
# sqlite3 gives a cryptic error if the target directory is missing.
mkdir -p /backup

# Safe to run while the application is running (WAL mode)
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    ".backup /backup/app-$(date +%Y%m%d-%H%M%S).db"
```

Add this to a cron job running at least daily:
```
0 3 * * * mkdir -p /backup && sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db ".backup /backup/app-$(date +\%Y\%m\%d-\%H\%M\%S).db" && find /backup -name 'app-*.db' -mtime +30 -delete
```

Note: `\%` is required in crontab — cron treats a bare `%` as a newline character.
This escaping is only needed inside the crontab entry. When testing the sqlite3
`.backup` command directly in a terminal, use `%` without the backslash.

---

## 10. Monitoring and alerting

### Recommended checks

Set up external monitoring (UptimeRobot, Grafana, or similar) for:

| Check | URL / command | Expected | Alert if |
|---|---|---|---|
| Application liveness | `GET /health` | HTTP 200, body `{"status":"ok"}` | Non-200 for 2+ minutes |
| TLS certificate | HTTPS handshake | Valid cert, >14 days | Expiry within 14 days |
| Storage usage | `df /mnt/nfs/ferri` | < 80% | > 80% used |
| Container health | `docker compose ps` | `healthy` | `unhealthy` or `exited` |
| Failed mail count | Admin UI or DB query | 0 | > 0 for 30+ minutes |

### Checking failed mails from the command line

```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT COUNT(*) FROM mail_queue WHERE status='failed';"
```

Add to a cron job that alerts if the count is non-zero. Two options depending on what is available on your host:

**Option A — write to a log file (simplest, no dependencies):**
```bash
#!/bin/bash
# Save as /opt/ferri/check-mail-queue.sh and make executable: chmod +x

DB="/var/lib/docker/volumes/filetransfer_app_db/_data/app.db"
LOG="/var/log/ferri-alerts.log"

COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM mail_queue WHERE status='failed';")

if [ "$COUNT" -gt "0" ]; then
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) WARNING: $COUNT failed mails in queue" >> "$LOG"
fi
```
Monitor `$LOG` with your existing log tooling (Grafana, Loki, or `tail -f`).

**Option B — send via curl to a webhook (Slack, Teams, or similar):**
```bash
#!/bin/bash
# Save as /opt/ferri/check-mail-queue.sh and make executable: chmod +x

DB="/var/lib/docker/volumes/filetransfer_app_db/_data/app.db"
WEBHOOK="https://hooks.slack.com/services/YOUR/WEBHOOK/URL"

COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM mail_queue WHERE status='failed';")

if [ "$COUNT" -gt "0" ]; then
    MSG="WARNING: ${COUNT} failed mails in ferri queue"
    curl -s -X POST "$WEBHOOK" \
        -H "Content-Type: application/json" \
        -d "{\"text\":\"${MSG}\"}"
fi
```

**Note:** The `mail` command (used in some examples online) is rarely installed on minimal server installations. The options above require only `sqlite3` and `curl`, both of which are standard.

Add to crontab (`crontab -e`):
```
*/15 * * * * /opt/ferri/check-mail-queue.sh
```

### Storage growth monitoring

```bash
# Add to daily cron
df -h /mnt/nfs/ferri | awk 'NR==2 {gsub(/%/,""); if ($5 > 80) print "WARNING: NFS storage at " $5 "% capacity"}'
```

### Log-based alerting

The application logs structured JSON. Watch for these patterns:

```bash
# Error log tail
docker compose logs -f | grep -i '"level":"error"'

# Filesystem errors (cleanup job failures)
docker compose logs | grep "filesystem error"

# Startup problems
docker compose logs | grep -iE "failed|panic|fatal"
```

---

## 11. Log reference

All logs are written to stdout/stderr and captured by Docker. View with:

```bash
docker compose logs                    # all logs
docker compose logs --tail=100         # last 100 lines
docker compose logs -f                 # follow (live)
docker compose logs --since=1h         # last hour
```

### Log format

```json
{"time":"2026-05-16T14:32:01Z","level":"INFO","msg":"transfer activated","transfer_id":"abc123","files":3,"recipients":2}
```

### Key log messages

| Message | Level | Meaning |
|---|---|---|
| `server listening on 0.0.0.0:8080` | INFO | Normal startup |
| `migrations complete (N applied)` | INFO | DB schema up to date |
| `startup: resetting N stuck mail_queue rows` | INFO | Recovered mails from previous crash. N > 0 means the previous process crashed during a send. |
| `transfer activated` | INFO | All files uploaded, recipients notified |
| `expiry job: N transfers expired` | INFO | Normal expiry job run |
| `cleanup job: N transfers cleaned, X GB freed` | INFO | Normal cleanup |
| `mail sent` | INFO | Successful mail delivery |
| `mail failed, attempt N/5` | WARN | SMTP error, will retry |
| `mail permanently failed` | ERROR | Exhausted retries — check admin UI |
| `filesystem error removing ...` | ERROR | Cleanup could not delete a file — check NFS connectivity |
| `tus validation failed` | WARN | Someone tried to upload without a valid token |
| `panic recovered` | ERROR | Application bug — copy the full stack trace and file an issue |

---

## 12. Troubleshooting

### Container won't start

```bash
docker compose logs
```

**"permission denied" on /data/storage:**
The container user (UID 1000) cannot write to the NFS mount.
```bash
# On the NFS server, ensure the export allows UID 1000
# Or on the host:
chown -R 1000:1000 /mnt/nfs/ferri
```

**"no such file or directory" for config.yaml:**
The config file is not where docker-compose.yml expects it.
```bash
ls -la config.yaml   # must exist in the same directory as docker-compose.yml
```

**"address already in use":**
Port 8080 is occupied by another process.
```bash
ss -tlnp | grep 8080
```

### Uploads fail or stall

**Large uploads drop mid-way:**
Check the reverse proxy timeout configuration. For nginx, ensure `proxy_read_timeout 0` and `proxy_send_timeout 0`. For Caddy, no changes are needed by default — it does not impose upload timeouts.

**"413 Request Entity Too Large" from nginx:**
Set `client_max_body_size 0` in your nginx config.

**Upload progress stalls at 0%:**
Check that `proxy_request_buffering off` is set in nginx. Without it, nginx buffers the entire body before forwarding, which blocks TUS progress events.

**TUS resume not working after restart:**
The `.info` sidecar files must be present on the NFS share. If they were deleted manually, the upload cannot be resumed — the user must start a new upload.

### Mail is not being sent

**Check the queue:**
```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT status, COUNT(*), MAX(error_message) FROM mail_queue GROUP BY status;"
```

**Test SMTP connectivity from the host:**
```bash
# The app runs in a distroless container — no shell or tools exist inside it.
# Run network tests from the host:
nc -zv mail.smtp2go.com 587
# Expected: Connection to mail.smtp2go.com 587 port [tcp/submission] succeeded!

# If nc is not installed, use bash's built-in /dev/tcp (no extra tools needed):
timeout 5 bash -c 'cat < /dev/null > /dev/tcp/mail.smtp2go.com/587' && echo "port reachable" || echo "port unreachable or timed out"
# This works on any Linux host with bash, without installing anything.
```

**If the SMTP connection succeeds but mails still fail:**
Check the `error_message` column in `mail_queue` for the exact SMTP error. Common causes:
- Wrong username or password → check `.env`
- From-address not verified in SMTP2GO → verify the domain in your SMTP2GO account
- Rate limiting → check your SMTP2GO usage dashboard

**Reset all failed mails to retry:**
```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "UPDATE mail_queue SET status='pending', attempts=0, next_attempt_at=unixepoch()
     WHERE status='failed';"
```

### Downloads fail for recipients

**"Transfer not found" or 404:**
- The transfer may have expired. Check in the admin UI.
- The token in the URL may be corrupted (e.g. stripped by a mail client). Ask the recipient to click the link directly rather than copying it.

**Download starts but stops part-way:**
- Check NFS connectivity: `df -h /mnt/nfs/ferri`
- Check NFS server health on the Dell PowerScale
- Large downloads over slow connections may time out at the reverse proxy — for nginx, ensure `proxy_read_timeout 0`

**"Incorrect password":**
The password is case-sensitive. If the sender forgot the password, an admin can clear it:
```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "UPDATE transfers SET password_hash=NULL WHERE id='<transfer_id>';"
```

### Admin UI inaccessible

**Redirect loop or "forbidden" from internal network:**
The IP allowlist may not include your workstation's IP. Check:
```bash
# Find your workstation's internal IP — this is what the ip_allowlist checks.
# (api.ipify.org gives your router's public internet IP, which is NOT relevant here.)
hostname -I | awk '{print $1}'
# or, more explicitly:
ip route get 1.1.1.1 | grep -oP 'src \K\S+'
# Note: 'awk {print $7}' is not reliable across all Linux distributions and
# iproute2 versions — the field position varies. The grep pattern is stable.

# Then check whether this IP falls within your configured ip_allowlist ranges.
# Example: if your IP is 10.0.5.42, it matches the range 10.0.0.0/8.

# Alternatively, check the server logs for the IP that was rejected:
docker compose logs | grep -i "ip\|forbidden\|allowlist" | tail -20
```

Update `ip_allowlist` in `config.yaml` and restart.

**Session cookie expired:**
Log in again at `/admin/login`. Increase `admin.session_ttl_hours` in config if this happens too often.

### Database issues

**"database is locked":**
This should not happen in normal operation (single-writer WAL mode). If it does, the application may have crashed mid-write. Stop the container and run:
```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db "PRAGMA integrity_check;"
# Expected: ok
```

If integrity check fails, restore from the most recent Litestream backup.

**Database is growing unexpectedly:**
`download_events` and `mail_queue` (sent rows) are the most likely sources of growth. Check size per table using SQLite's built-in `dbstat` virtual table:
```bash
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT name,
            SUM(pgsize) / 1024 / 1024 AS size_mb
     FROM dbstat
     WHERE aggregate = TRUE
     GROUP BY name
     ORDER BY size_mb DESC;"
```

Sent mail rows are automatically pruned after `mail_retention_days` (default 90). Download events are kept indefinitely — they are audit data and are small (a few hundred bytes per event).

---

## 13. Security checklist

Run through this checklist after installation and after any significant configuration change.

### Network
- [ ] Server is in a DMZ, isolated from internal network
- [ ] Firewall blocks all outbound connections from server to internal network
- [ ] Firewall allows only ports 443 (inbound) and 587 (outbound to SMTP relay) and 2049 (to NFS)
- [ ] Firewall rules verified with test connections (see §4)

### Application
- [ ] `ADMIN_TOKEN` is at least 32 random characters and stored only in `.env`
- [ ] `.env` is not committed to version control
- [ ] `config.yaml` does not contain any passwords (all secrets via env vars)
- [ ] `ip_allowlist` is set to the minimum necessary CIDR ranges
- [ ] `limits.max_upload_bytes` is set appropriately (avoid setting to 0 = unlimited)
- [ ] Admin UI is accessible only from internal network (test from external network)
- [ ] A test transfer was sent and the notification mail was received

### TLS
- [ ] TLS certificate is valid and not expiring within 30 days
- [ ] HTTPS redirect is working (HTTP → HTTPS)
- [ ] HSTS header is set by reverse proxy

### Container
- [ ] Container runs as non-root (`user: "1000:1000"`)
- [ ] `read_only: true` is set
- [ ] `cap_drop: [ALL]` is set
- [ ] Image version is pinned (not `:latest`) in `docker-compose.yml`
- [ ] Litestream version is pinned (not `:latest`) in `Dockerfile`

### Backup
- [ ] Litestream replication is active (check logs)
- [ ] A test restore was performed on a separate host (ideally at first install)
- [ ] NFS share is covered by the PowerScale snapshot or backup policy

---

*Last updated: 2026-05-16. Update this document when the deployment changes meaningfully.*
