# Ferri

Self-hosted file transfer tool. Send large files to external recipients, or request files from them — all via a simple web interface, with resumable uploads and no size limits beyond your storage capacity.

---

## Features

- **Send files** — upload files and send a download link to one or more recipients by email, or copy the link yourself
- **Request files** — generate an upload link to send to an external party; receive a notification when they're done
- **Resumable uploads** — uses the [TUS protocol](https://tus.io); large files (tested up to 600 GB) survive interrupted connections
- **No client software** — works entirely in the browser
- **Self-hosted** — your files stay on your own storage
- **SMB/CIFS and local storage** — configure your storage backend via the admin panel; hot-swap without restart
- **Branding** — configurable company name, logo, and colours via the admin panel
- **Mail notifications** — recipients notified by email; sender notified on download; expiry summary on transfer expiry
- **IP restriction** — only internal network users can create transfers; download links are publicly accessible
- **Automatic cleanup** — expired transfers are removed from storage on a configurable schedule

---

## Prerequisites

- Docker Engine 24.0 or later
- Docker Compose v2 (`docker compose`, not `docker-compose`)
- An SMTP relay (e.g. SMTP2GO, Postfix, Office 365 SMTP relay)
- Storage: a local path, or an SMB/CIFS share (configured via the admin panel after first run)

---

## Quick start

### 1 — Clone

```bash
git clone https://github.com/BartVermeir/Ferri.git
cd Ferri
```

### 2 — Create secrets

```bash
cp .env.example .env
```

Edit `.env`:

```env
ADMIN_TOKEN=<generate with: openssl rand -base64 32>
SMTP_PASSWORD=<your SMTP relay password>
```

**Never commit `.env` to version control.**

### 3 — Create configuration

```bash
cp config.example.yaml config.yaml
cp litestream.example.yml litestream.yml
```

`litestream.yml` is required — `docker-compose.yml` mounts it unconditionally. If you don't need database backups, edit the file and remove the `replicas:` block entirely. See `litestream.example.yml` for S3 and local-file replica options.

Minimum required edits in `config.yaml`:

```yaml
server:
  base_url: "https://send.example.com"   # your public URL

smtp:
  host: "mail.smtp2go.com"
  username: "your_smtp_username"
```

If you're running behind a reverse proxy, also set:

```yaml
server:
  trusted_proxies:
    - "127.0.0.1/32"     # the IP of your proxy — prevents clients from spoofing X-Forwarded-For
```

See `config.example.yaml` for all options with annotations.

### 4 — Edit storage path and build

Open `docker-compose.yml` and replace the storage volume with your own path:

```yaml
volumes:
  - /your/storage/path:/data/storage    # ← change this
```

Then build and start:

```bash
docker build -t ferri:latest .
docker compose up -d
```

The application listens on `localhost:8080`. Put a reverse proxy (Caddy or nginx) in front for TLS. See `docs/operations.md` for a complete reverse proxy configuration including the TUS-specific headers required for large file uploads.

### 5 — First login

Navigate to `http://localhost:8080/admin/login` and enter your `ADMIN_TOKEN`.

From the admin panel you can configure branding, mail settings, and connect your storage backend.

---

## Documentation

| Document | Contents |
|---|---|
| `docs/operations.md` | Full installation guide, configuration reference, reverse proxy setup, monitoring, troubleshooting |
| `docs/architecture.md` | System design, data flows, component descriptions |
| `docs/DECISIONS.md` | Architectural decisions and their rationale |
| `docs/update-guide.md` | Update and rollback procedures |

---

## Configuration overview

| File | Purpose |
|---|---|
| `.env` | Secrets (ADMIN_TOKEN, SMTP_PASSWORD). Never commit this. |
| `config.yaml` | All other configuration. Copy from `config.example.yaml`. |
| `litestream.yml` | SQLite replication config. Copy from `litestream.example.yml`. Required — docker-compose mounts it. |
| `docker-compose.yml` | Container setup. Edit the storage volume mount. |

---

## Storage

By default the container writes to `/data/storage`. Mount your storage there in `docker-compose.yml`:

```yaml
volumes:
  - /path/to/your/storage:/data/storage
```

SMB/CIFS storage can be configured directly in the admin panel under Settings → Storage, without editing any config files or restarting the container.

---

## License

MIT — see `LICENSE`.
