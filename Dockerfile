# ── Stage 1: build ────────────────────────────────────────────────────────────
# Pinned patch release: must match the toolchain line in go.mod.
FROM golang:1.26.8-alpine AS builder

WORKDIR /app

# VERSION is the release tag of this build (set by scripts/deploy.sh),
# shown in the admin panel footer.
ARG VERSION=dev

# Download dependencies first (cached layer).
# 'go mod download' needs a complete go.sum ('go mod tidy').
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
# CGO_ENABLED=0 required for modernc/sqlite (pure Go, no C dependency)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.Version=${VERSION}" -o /ferri ./cmd/server

# Create the /data directory owned by UID 1000.
# This is copied into the runtime image and then into the named volume on first
# Docker mount (Docker initialises empty named volumes from image contents).
# Done in this stage because distroless has no shell.
RUN mkdir -p /data && chown 1000:1000 /data

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
# Distroless: minimal attack surface, no shell, no package manager
FROM gcr.io/distroless/static-debian12

# Copy the application binary
COPY --from=builder /ferri /ferri

# Copy the pre-created /data directory with ownership 1000:1000, which an
# empty app_db volume takes over on first mount.
COPY --chown=1000:1000 --from=builder /data /data

# Run as non-root user (UID 1000)
# The NFS storage mount must allow writes from this UID
USER 1000:1000

ENTRYPOINT ["/ferri"]
