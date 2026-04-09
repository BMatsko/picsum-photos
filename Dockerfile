# ─── Build stage ─────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache \
    gcc \
    musl-dev \
    vips-dev \
    git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# Add pgx driver (safe to run even if already in go.sum)
RUN go get github.com/jackc/pgx/v5 && go mod tidy || true

COPY . .
RUN go build -o /bin/picsum-photos ./cmd/picsum-photos

# ─── Runtime stage ────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache vips

COPY --from=builder /bin/picsum-photos /bin/picsum-photos

EXPOSE 8080

# Required env vars (set in Railway Variables tab):
#   DATABASE_URL        — auto-injected by Railway Postgres plugin
#   PICSUM_HMAC_KEY     — any random secret string
#
# Optional env vars:
#   PICSUM_ROOT_URL     — your public Railway URL (auto-detected if not set)
#   PICSUM_STORAGE_PATH — defaults to /data/images
#   PORT                — auto-set by Railway

CMD ["/bin/picsum-photos"]
