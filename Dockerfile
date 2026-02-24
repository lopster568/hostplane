# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o control-plane .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.21

# cloudflared CLI is used by TunnelManager for DNS route management
RUN apk add --no-cache ca-certificates curl && \
    curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 \
         -o /usr/local/bin/cloudflared && \
    chmod +x /usr/local/bin/cloudflared

WORKDIR /app
COPY --from=builder /src/control-plane .

ENTRYPOINT ["/app/control-plane"]
