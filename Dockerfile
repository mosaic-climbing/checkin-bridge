# ── Build stage ────────────────────────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /mosaic-bridge ./cmd/bridge

# ── Runtime stage (scratch = zero dependencies, ~8MB image) ─
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /mosaic-bridge /mosaic-bridge
# NOTE: .env is NOT baked in. Mount it at runtime:
#   docker run -v /host/path/.env:/.env:ro -p 3500:3500 mosaic-bridge
# or pass individual env vars with --env-file /host/path/.env.
ENTRYPOINT ["/mosaic-bridge"]
