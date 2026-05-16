# update-guide.md

This document describes how to update the application, its dependencies, and its infrastructure components. It covers the full update lifecycle: preparation, execution, verification, and rollback.

Read this document completely before starting any update. Updates that seem simple (a one-line config change) can have non-obvious consequences.

For day-to-day operations, see `operations.md`. For architectural decisions, see `architecture.md`.

---

## Table of contents

1. [Before any update](#1-before-any-update)
2. [Updating the application](#2-updating-the-application)
3. [Database schema migrations](#3-database-schema-migrations)
4. [Updating Litestream](#4-updating-litestream)
5. [Updating the base Docker image](#5-updating-the-base-docker-image)
6. [Updating the reverse proxy](#6-updating-the-reverse-proxy)
7. [Updating config.yaml](#7-updating-configyaml)
8. [Rollback procedures](#8-rollback-procedures)
9. [Update log](#9-update-log)

---

## 1. Before any update

Every update — regardless of how small — requires the same preparation steps. Do not skip them.

### Step 1 — Verify backups are current

Before touching anything, confirm that Litestream is replicating and that the most recent backup is recent:

```bash
# Check Litestream is active
docker compose logs | grep -i litestream | tail -10
# Expected: recent snapshot or WAL segment entries, no errors

# For S3 replicas: confirm recent files exist
aws s3 ls s3://your-backup-bucket/ferri/db/ --recursive \
    | sort | tail -5
# Expected: files timestamped within the last few minutes
```

If backups are not current, do not proceed. Fix Litestream first.

### Step 2 — Note the current state

Record what is running before you change anything:

```bash
# Current image tag, status, and health — save this output before proceeding
docker compose ps --format "table {{.Name}}\t{{.Image}}\t{{.Status}}\t{{.Health}}"
# The Image column shows the exact tag that is running.
# Record this — you will need it if you need to roll back.

# Current database row counts (sanity check after update)
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT 'transfers', COUNT(*) FROM transfers
     UNION ALL SELECT 'files', COUNT(*) FROM files
     UNION ALL SELECT 'recipients', COUNT(*) FROM recipients
     UNION ALL SELECT 'mail_queue', COUNT(*) FROM mail_queue;"
```

Save this output. You will compare it after the update to confirm nothing was lost.

### Step 3 — Check for active uploads and downloads

Avoid updating during peak hours or while large transfers are in progress:

```bash
# Active transfers (pending = upload in progress)
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT COUNT(*) FROM transfers WHERE status = 'pending';"

# Check logs for recent TUS activity
docker compose logs --since=10m | grep -iE "tus|upload|patch" | tail -20
```

If there are active uploads, wait for them to complete or warn users before proceeding. A container restart interrupts in-progress TUS uploads — the TUS protocol supports resumption, so users can retry, but it is disruptive.

### Step 4 — Take a manual database snapshot

Even with Litestream running, take an explicit snapshot immediately before the update:

```bash
mkdir -p /backup/pre-update
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    ".backup /backup/pre-update/app-$(date +%Y%m%d-%H%M%S).db"
echo "Pre-update snapshot complete: $(ls -lh /backup/pre-update/ | tail -1)"
```

---

## 2. Updating the application

The application is a Go binary built into a Docker image. Updating means building a new image and replacing the running container.

### Full update procedure

```bash
# 1. Pull the latest code
cd /opt/ferri   # or wherever you cloned the repo
git fetch --tags
git log --oneline HEAD..origin/main | head -10   # see what changed
git pull origin main

# 2. Review the changelog before building
# Check CHANGELOG.md or git log for breaking changes,
# new config keys, or schema migrations.
head -50 CHANGELOG.md

# 3. Build the new image with an explicit version tag
#    Use the git tag or today's date — never 'latest' in production
VERSION=$(git describe --tags --always)
docker build -t ferri:${VERSION} .
echo "Built: ferri:${VERSION}"

# 4. Update docker-compose.yml to use the new tag
sed -i "s|image: ferri:.*|image: ferri:${VERSION}|" docker-compose.yml
CHANGED=$(grep -c "image: ferri:${VERSION}" docker-compose.yml)
echo "Updated ${CHANGED} image reference(s) — expected 1"
grep "image:" docker-compose.yml   # confirm the change
# If CHANGED > 1, multiple services matched. Edit docker-compose.yml manually
# to ensure only the 'app' service image was updated.

# 5. Stop the current container gracefully
#    If stop_grace_period is set, Docker waits that long for active connections.
docker compose stop
echo "Container stopped at $(date)"

# 6. Start with the new image
docker compose up -d

# 7. Wait for healthy status, then show recent logs
echo "Waiting for container to become healthy..."
for i in $(seq 1 12); do
    sleep 5
    STATUS=$(docker compose ps --format "{{.Health}}" 2>/dev/null | head -1)
    if [ "$STATUS" = "healthy" ]; then
        echo "Container healthy after $((i * 5)) seconds"
        break
    fi
    if [ $i -eq 12 ]; then
        echo "WARNING: container did not become healthy within 60 seconds"
        break
    fi
    echo "  Still waiting... ($((i * 5))s elapsed, status: ${STATUS:-starting})"
done
docker compose logs --tail=30
```

### Post-update verification

Run these checks immediately after starting the new container:

```bash
# Container is healthy
docker compose ps
# Expected: running (healthy)

# Application responds
curl -s https://send.example.com/health
# Expected: {"status":"ok"}

# Row counts match pre-update snapshot
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT 'transfers', COUNT(*) FROM transfers
     UNION ALL SELECT 'files', COUNT(*) FROM files
     UNION ALL SELECT 'recipients', COUNT(*) FROM recipients
     UNION ALL SELECT 'mail_queue', COUNT(*) FROM mail_queue;"
# Expected: same counts as before, or higher (new activity during update window)

# No errors in logs
docker compose logs --since=5m | grep -iE '"level":"error"|panic|fatal'
# Expected: no output
```

If any check fails, see §8 Rollback procedures.

---

## 3. Database schema migrations

Schema migrations run automatically at startup. The application reads pending migration files from `internal/db/migrations/` in filename order and applies any that have not yet run. This is tracked in a `schema_migrations` table in SQLite.

### What this means for updates

- **You do not need to run migrations manually.** Starting the new container is sufficient.
- **Migrations are forward-only.** There is no automatic down-migration. Rollback requires restoring the pre-update database snapshot (see §8).
- **Migrations run before the HTTP server starts.** If a migration fails, the container exits immediately with an error logged. No requests are served until migrations succeed.

### Checking migration status

```bash
# See which migrations have been applied and when
sqlite3 /var/lib/docker/volumes/filetransfer_app_db/_data/app.db \
    "SELECT filename, applied_at, datetime(applied_at, 'unixepoch') AS applied
     FROM schema_migrations
     ORDER BY applied_at;"
```

### Adding a new migration (for developers)

1. Create a new file in `internal/db/migrations/` with the next sequential number:
   `002_add_column_x.sql`, `003_add_table_y.sql`, etc.
2. Write only forward SQL — no `DROP` statements unless you are dropping something added in the same migration.
3. Test on a copy of the production database before deploying.
4. The migration filename must sort lexicographically after all existing migrations. Zero-pad the number: `002`, not `2`.

### Schema migration rules

- Never modify an existing migration file that has already been applied to production. Create a new migration instead.
- Never rename a migration file that has been applied. The tracker uses the filename.
- Migrations must be idempotent where possible — use `IF NOT EXISTS`, `IF EXISTS`, `INSERT OR IGNORE`.
- Test every migration against a copy of the production database before shipping.

---

## 4. Updating Litestream

Litestream is embedded in the Docker image (copied from `litestream/litestream:0.3.13` during build). Updating Litestream means rebuilding the application image with a new Litestream version.

**Why we pin Litestream:** The container runs with `read_only: true`. Litestream must write only to `/data` (named volume) and `/tmp` (tmpfs). A new Litestream version that writes to an additional path would cause a silent failure at startup. See DECISIONS.md DEC-025.

### Procedure

```bash
# 1. Check the Litestream release notes for breaking changes
#    https://github.com/benbjohnson/litestream/releases

# 2. Update the version in the Dockerfile
#    Change: FROM litestream/litestream:0.3.13
#    To:     FROM litestream/litestream:0.3.X  (new version)
sed -i 's|litestream/litestream:.*|litestream/litestream:0.3.X|' Dockerfile
# Replace 0.3.X with the actual new version

# 3. Build the new image
#    Define VERSION here in case you jumped directly to this section
#    without having run the full update procedure in §2 first.
VERSION=$(git describe --tags --always)
docker build -t ferri:${VERSION}-ls-updated .

# 4. Test in a staging environment if possible

# 5. Deploy using the standard update procedure (§2)

# 6. After starting, verify Litestream is replicating
docker compose logs | grep -i litestream | tail -20
# Expected: "replication started" followed by "snapshot written" or "wal segment written"
# If you only see "replication started" and nothing after 60 seconds, check for errors.

# 7. Verify via the health check — distroless has no shell tools, use curl instead
curl -s https://send.example.com/health
# Expected: {"status":"ok"}
```

**If the new Litestream version fails to start:**

```bash
docker compose logs | grep -iE "error|permission|read-only"
```

Common cause: the new version writes to a path that is not `/data` or `/tmp`. Add the path as a `tmpfs` entry in `docker-compose.yml`, or revert to the previous Litestream version.

---

## 5. Updating the base Docker image

The runtime image is `gcr.io/distroless/static-debian12`. This rarely needs updating, but security patches may require it.

```bash
# Define VERSION here in case you jumped directly to this section
VERSION=$(git describe --tags --always)

# 1. Pull the latest distroless image
docker pull gcr.io/distroless/static-debian12:latest

# 2. Rebuild the application image — the build stage uses golang:1.23-alpine
#    Check for a newer Go version if needed
docker build --no-cache -t ferri:${VERSION}-rebase .

# 3. Verify the new image size is reasonable
docker images ferri
# A distroless Go binary image should be 10-30MB

# 4. Deploy using the standard update procedure (§2)
```

Go version updates (e.g. 1.23 → 1.24) require changing the `FROM golang:1.23-alpine` line in the Dockerfile and rebuilding. Check the Go release notes for any breaking changes in the standard library before upgrading.

---

## 6. Updating the reverse proxy

### Caddy

Caddy auto-updates its TLS certificates. The Caddy binary itself is managed by the system package manager:

```bash
apt update && apt upgrade caddy
systemctl status caddy   # verify it is still running after upgrade
```

After upgrading, test TUS uploads work correctly — Caddy version updates occasionally change how large request bodies are handled:

```bash
# Verify CORS headers are present (critical for TUS)
curl -si -X OPTIONS https://send.example.com/tus/ \
    -H "Origin: https://send.example.com" \
    -H "Access-Control-Request-Method: PATCH" | grep -i "access-control"
# Expected: Access-Control-Allow-Origin, Access-Control-Allow-Methods, etc.
```

### nginx

```bash
apt update && apt upgrade nginx
nginx -t   # test config before reloading
systemctl reload nginx
```

After upgrading, verify the critical settings are still in effect:

```bash
# Check client_max_body_size is not being overridden
nginx -T | grep -E "client_max_body_size|proxy_request_buffering"
# Expected: client_max_body_size 0; proxy_request_buffering off;
```

---

## 7. Updating config.yaml

Config changes require a container restart. They do not require a new image build.

```bash
# 1. Edit config.yaml
nano /opt/ferri/config.yaml

# 2. Validate the YAML syntax before restarting.
#    PyYAML is required — install if not present:
#    pip3 install pyyaml --break-system-packages
python3 -c "import yaml; yaml.safe_load(open('config.yaml'))" && echo "YAML valid"
# If this prints a YAML error, fix the syntax before restarting.
# If this prints 'No module named yaml', install PyYAML first (see above).

# 3. Restart the container
docker compose restart

# 4. Verify startup succeeded
docker compose ps
docker compose logs --tail=20
```

**Changes that require `docker compose restart`:**

- `smtp.*` settings (host, port, username, TLS mode)
- `ip_allowlist` entries
- `limits.*` (max upload size, max files)
- `jobs.*` interval settings (`mail_interval_minutes`, `expiry_interval_minutes`, `cleanup_interval_hours`, `stall_timeout_hours`, `mail_retention_days`, `cleanup_grace_hours`)
- `expiry_options` (the dropdown choices on the send form)
- `admin.session_ttl_hours`
- `server.base_url`, `server.trusted_proxies`

**Changes that require `docker compose down && docker compose up -d` instead of `restart`:**

- Adding or changing environment variables in `docker-compose.yml`
- Adding or changing volume mounts
- Changing `stop_grace_period`
- Changing the image tag

`docker compose restart` only restarts the container process — it does not re-apply changes to the compose file itself.

**Changes that do NOT require any restart:**

- Branding (colours, logo, company name) — via admin UI, takes effect within 60 seconds
- `mail.from_address`, `mail.from_name` — via admin UI
- `ui.welcome_message`, `ui.send_page_title`, `ui.download_page_title` — via admin UI

---

## 8. Rollback procedures

### Application rollback

If the new version is broken and needs to be reverted:

```bash
# 1. Stop the broken container
docker compose stop

# 2. Revert docker-compose.yml to the previous image tag
git diff docker-compose.yml   # see exactly what the update changed
# If this shows output: the new tag is uncommitted — git checkout -- will restore the old tag.
# If this shows NO output: the new tag was already committed before the update.
#   In that case, git checkout -- does nothing. Use instead:
#   git show HEAD~1:docker-compose.yml > docker-compose.yml

# Revert only docker-compose.yml to its last committed state.
# Using '--' only touches this one file, leaving any other uncommitted
# changes (e.g. config.yaml edits) untouched.
git checkout -- docker-compose.yml

# Or simply edit manually and set the known-good image tag:
# image: ferri:1.0.0

# 3. Start with the old image
docker compose up -d

# 4. Verify the old version is running and healthy
docker compose ps
curl -s https://send.example.com/health
```

**Keep old Docker images.** Do not prune images immediately after an update:

```bash
# List available ferri images
docker images ferri
# Keep at least the two most recent versions on disk
```

### Database rollback

If a schema migration caused data corruption or an incompatible change:

```bash
# 1. Stop the application immediately
docker compose stop

# 2. Restore the pre-update snapshot
#    The pre-update snapshot is at /backup/pre-update/
ls -lh /backup/pre-update/ 2>/dev/null || echo "(directory is empty or missing)"

SNAPSHOT=$(ls /backup/pre-update/app-*.db 2>/dev/null | sort | tail -1)
if [ -z "$SNAPSHOT" ]; then
    echo "ERROR: no pre-update snapshot found in /backup/pre-update/"
    echo "Use the Litestream emergency restore procedure at the end of this section."
    echo "Stop here — do not continue with the steps below."
    # Note: 'false' sets a non-zero exit code without closing your shell session.
    # 'exit 1' would close an interactive terminal — avoid it in copy-pasted commands.
    false
fi
echo "Restoring from: $SNAPSHOT"

# 3. Replace the live database with the snapshot
#    The database is at the app_db volume mount point
VOLUME_PATH=$(docker volume inspect filetransfer_app_db \
    | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['Mountpoint'])")

# Make a safety copy of the broken DB first
cp "${VOLUME_PATH}/app.db" "${VOLUME_PATH}/app.db.broken"

# Restore the snapshot
cp "$SNAPSHOT" "${VOLUME_PATH}/app.db"

# Remove WAL and SHM sidecar files left from the previous database session.
# SQLite in WAL mode creates app.db-wal and app.db-shm alongside the main file.
# If these are present when the restored database is opened, SQLite may attempt
# to apply WAL entries from the broken session to the clean snapshot,
# resulting in corruption. Always remove them after a restore.
rm -f "${VOLUME_PATH}/app.db-wal"
rm -f "${VOLUME_PATH}/app.db-shm"
echo "WAL and SHM files cleared"

# 4. Revert the application to the previous version (see above)

# 5. Start the application
docker compose up -d

# 6. Verify
curl -s https://send.example.com/health
sqlite3 "${VOLUME_PATH}/app.db" \
    "SELECT COUNT(*) FROM transfers; SELECT COUNT(*) FROM files;"
```

**What you lose with a database rollback:** Any transfers created, any downloads recorded, and any mail queue entries added between the snapshot and the rollback. Notify affected users manually if needed.

### Emergency: restore from Litestream backup

If the pre-update snapshot is not available or is itself corrupt, restore from Litestream:

```bash
# Stop the application
docker compose stop

# Find available restore points by listing the S3 backup prefix.
# The 'snapshots' subcommand does not exist in Litestream 0.3.x —
# use aws s3 ls to inspect what is available instead.
aws s3 ls s3://your-backup-bucket/ferri/db/ --recursive \
    | sort | grep snapshot
# Look for snapshot files with timestamps before the failed update.
# Note the timestamp you want to restore to.

# Restore to a specific timestamp (before the update)
VOLUME_PATH=$(docker volume inspect filetransfer_app_db \
    | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['Mountpoint'])")

docker run --rm \
  -v "${VOLUME_PATH}:/data" \
  -e LITESTREAM_ACCESS_KEY_ID=${LITESTREAM_ACCESS_KEY_ID} \
  -e LITESTREAM_SECRET_ACCESS_KEY=${LITESTREAM_SECRET_ACCESS_KEY} \
  litestream/litestream:0.3.13 \
  restore -o /data/app.db \
  -timestamp "YYYY-MM-DDTHH:MM:SSZ" \
  s3://your-backup-bucket/ferri/db
# Replace YYYY-MM-DDTHH:MM:SSZ with a timestamp from the aws s3 ls output above,
# chosen to be just before the failed update (e.g. "2026-05-16T08:00:00Z").

# Remove WAL and SHM sidecar files — same reason as the manual restore above.
# Stale WAL entries from the previous session can corrupt the restored database.
rm -f "${VOLUME_PATH}/app.db-wal"
rm -f "${VOLUME_PATH}/app.db-shm"
echo "WAL and SHM files cleared"

# Verify integrity and start
sqlite3 "${VOLUME_PATH}/app.db" "PRAGMA integrity_check;"
# Expected: ok
docker compose up -d
```

---

## 9. Update log

Keep a record of every update applied to this deployment. Copy and fill in a new entry each time.

```
## [DATE] — [DESCRIPTION]

- Previous version: ferri:X.Y.Z
- New version:      ferri:A.B.C
- Litestream:       0.3.13 (unchanged / updated to 0.3.X)
- Schema migrations applied: none / 002_add_column_x.sql
- Pre-update snapshot: /backup/pre-update/app-YYYYMMDD-HHMMSS.db
- Applied by: [name]
- Downtime: approximately N minutes (container restart)
- Issues encountered: none / [description]
- Rollback performed: no / yes — reason: [description]
```

---

*Last updated: 2026-05-16. Update this document when the update procedure changes meaningfully.*
