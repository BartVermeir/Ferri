# Ferri

Self-hosted file transfer tool
Supports resumable uploads (TUS), 500–600 GB files, NFS/ZFS storage, single administrator.

---

## Prerequisites

- Go 1.23+
- Docker + Docker Compose
- Access to an NFS share (or local path for development)

---

## First-time setup

After cloning the repository, populate `go.sum` before building:

```bash
go mod tidy
```

This is required once, and again whenever `go.mod` changes. Without it, `docker build` will fail with a checksum error.

---

## Configuration

```bash
cp config.example.yaml config.yaml
cp .env.example .env
cp litestream.example.yml litestream.yml
```

Edit `config.yaml` and `.env` with your values. See `config.example.yaml` for all options with annotations.

Generate a strong admin token:

```bash
openssl rand -base64 32
```

---

## Build and run

```bash
docker build -t ferri:latest .
docker compose up -d
```

---

## Documentation

| Document | Contents |
|---|---|
| `docs/architecture.md` | System design, data flows, component descriptions |
| `docs/DECISIONS.md` | All architectural decisions and their rationale |
| `docs/operations.md` | Installation, monitoring, troubleshooting |
| `docs/update-guide.md` | Update and rollback procedures |

---

## Development

All source code is in `internal/`. Stub handlers are in `internal/handler/stubs.go` — replace each stub with a real implementation as development progresses.

Recommended implementation order:
1. `internal/tus/handler.go` — resumable upload integration
2. `internal/handler/download.go` — file download with Range support
3. `internal/handler/send.go` — transfer creation
4. `internal/mail/mailer.go` — SMTP client + templates
5. `internal/handler/admin.go` — admin UI
