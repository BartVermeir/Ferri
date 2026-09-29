# ── Stage 1: build ────────────────────────────────────────────────────────────
# Pinned patch release: must match the toolchain line in go.mod.
FROM golang:1.26.8-alpine AS builder

WORKDIR /app

# VERSION should be the exact `git describe --tags` output for this build
# (the deploy script sets it automatically) — this is what shows up in the
# admin panel footer, so a running container's version is always visible.
ARG VERSION=dev

# Download dependencies first (cached layer).
# go.sum must be populated by running 'go mod tidy' before building.
# An empty go.sum will cause 'go mod download' to fail.
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
# CGO_ENABLED=0 required for modernc/sqlite (pure Go, no C dependency)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.Version=${VERSION}" -o /ferri ./cmd/server

# Create the /data directory owned by UID 1000.
# This is copied into the runtime image and then into the named volume on first
# Docker mount (Docker initialises empty named volumes from image contents).
# distroless has no shell, so RUN is impossible there — we do it here instead.
RUN mkdir -p /data && chown 1000:1000 /data

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
# Distroless: minimal attack surface, no shell, no package manager
FROM gcr.io/distroless/static-debian12

# Copy the application binary
COPY --from=builder /ferri /ferri

# Copy the pre-created /data directory with correct ownership (1000:1000).
# COPY --chown is handled by the Docker daemon — no shell required.
# Docker initialises empty named volumes from image directory contents on first mount,
# so this gives UID 1000 write access to the app_db volume without root at runtime.
COPY --chown=1000:1000 --from=builder /data /data

# Run as non-root user (UID 1000)
# The NFS storage mount must allow writes from this UID
USER 1000:1000

ENTRYPOINT ["/ferri"]
