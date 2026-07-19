# syntax=docker/dockerfile:1
#
# Multi-stage build. Pure Go (CGO_ENABLED=0), so cross-compiling for
# another target platform needs no C toolchain.
#   docker buildx build --platform linux/arm64 --target production -t <ref> .
ARG GO_VERSION=1.26.3

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/universal-core ./cmd/universal-core

# ------------------------------------------------------------------
FROM debian:bookworm-slim AS production

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --create-home --home-dir /app appuser

WORKDIR /app
COPY --from=builder /out/universal-core /app/universal-core

# cmd/universal-core's binary doesn't read migrations or locale files off
# disk yet (main.go currently just DB-pings and serves /healthz — the
# migration-applying and i18n-loading the package doc comment describes
# aren't wired in yet) — nothing else to COPY until it does.
ENV LISTEN_ADDR=:8090

USER 10001
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD curl -fsS http://localhost:8090/healthz || exit 1

ENTRYPOINT ["/app/universal-core"]
