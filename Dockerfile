# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Download dependencies first (cached layer).
# go.sum must be populated by running 'go mod tidy' before building.
# An empty go.sum will cause 'go mod download' to fail.
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
# CGO_ENABLED=0 required for modernc/sqlite (pure Go, no C dependency)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /ferri ./cmd/server

# Create the /data directory owned by UID 1000.
# This is copied into the runtime image and then into the named volume on first
# Docker mount (Docker initialises empty named volumes from image contents).
# distroless has no shell, so RUN is impossible there — we do it here instead.
RUN mkdir -p /data && chown 1000:1000 /data

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
# Distroless: minimal attack surface, no shell, no package manager
FROM gcr.io/distroless/static-debian12

# Copy Litestream for continuous SQLite replication
# Version is pinned — see DECISIONS.md DEC-025
COPY --from=litestream/litestream:0.3.13 /usr/local/bin/litestream /litestream

# Copy the application binary
COPY --from=builder /ferri /ferri

# Copy the pre-created /data directory with correct ownership (1000:1000).
# COPY --chown is handled by the Docker daemon — no shell required.
# Docker initialises empty named volumes from image directory contents on first mount,
# so this gives UID 1000 write access to the app_db volume without root at runtime.
COPY --chown=1000:1000 --from=builder /data /data

# Copy the example Litestream config as a fallback default.
# In production, mount your actual litestream.yml over this path:
#   volumes:
#     - ./litestream.yml:/etc/litestream.yml:ro
# Without a volume mount, Litestream uses this example config (local file replica only).
COPY litestream.example.yml /etc/litestream.yml

# Run as non-root user (UID 1000)
# The NFS storage mount must allow writes from this UID
USER 1000:1000

# Litestream wraps the application:
# - Starts replication before the app starts
# - If Litestream exits, the app exits too (Docker restarts both)
# - Credentials via LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY env vars
ENTRYPOINT ["/litestream", "replicate", "-exec", "/ferri"]
