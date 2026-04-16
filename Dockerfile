# ─── Build stage ─────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache \
    gcc \
    musl-dev \
    vips-dev \
    vips-webp \
    libheif-dev \
    git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /bin/picsum-photos ./cmd/picsum-photos

# ─── Runtime stage ────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache vips vips-webp vips-heif

COPY --from=builder /bin/picsum-photos /bin/picsum-photos

EXPOSE 8080

# Required env vars (set in Railway Variables tab):
#   DATABASE_URL           — auto-injected by Railway Postgres plugin
#   PICSUM_HMAC_KEY        — any random secret string
#
# Optional env vars:
#   PICSUM_ROOT_URL        — your public Railway URL (auto-detected if not set)
#   PICSUM_STORAGE_PATH    — local fallback storage path (default: /data/images)
#   PORT                   — auto-set by Railway
#
# SFTP storage (all required together to enable):
#   PICSUM_SFTP_HOST       — SFTP server hostname, e.g. sftp.example.com
#   PICSUM_SFTP_PORT       — SFTP port (default: 22)
#   PICSUM_SFTP_USER       — SFTP username
#   PICSUM_SFTP_PASSWORD   — SFTP password
#   PICSUM_SFTP_PATH       — base directory on server (default: /images)

CMD ["/bin/picsum-photos"]
