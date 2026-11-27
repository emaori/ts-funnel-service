# syntax=docker/dockerfile:1

ARG ALPINE_VERSION=3.24.2
ARG TAILSCALE_VERSION=v1.104.1
ARG CADDY_VERSION=2.11.4
ARG GO_VERSION=1.27.4

FROM tailscale/tailscale:${TAILSCALE_VERSION} AS tailscale
FROM caddy:${CADDY_VERSION}-alpine AS caddy

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG SERVICE_VERSION=development
ARG TAILSCALE_VERSION
ARG CADDY_VERSION
ARG ALPINE_VERSION
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w -X main.serviceVersion=${SERVICE_VERSION} -X main.tailscaleVersion=${TAILSCALE_VERSION} -X main.caddyVersion=${CADDY_VERSION} -X main.alpineVersion=${ALPINE_VERSION}" \
      -o /out/ts-funnel ./cmd/ts-funnel

# Unprivileged userspace networking: no TUN, NET_ADMIN, package installation,
# runtime compiler, Docker socket, or external monitoring backend is needed.
FROM alpine:${ALPINE_VERSION}
ARG TAILSCALE_VERSION
ARG CADDY_VERSION
ARG ALPINE_VERSION
ARG UID=1000
ARG GID=1000

COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscale
COPY --from=tailscale /usr/local/bin/tailscaled /usr/local/bin/tailscaled
COPY --from=caddy /usr/bin/caddy /usr/local/bin/caddy
COPY --from=build /out/ts-funnel /usr/local/bin/ts-funnel

RUN addgroup -g "${GID}" tsfunnel \
 && adduser -D -H -u "${UID}" -G tsfunnel tsfunnel \
 && mkdir -p /var/lib/tailscale /var/run/tailscale /etc/caddy /var/lib/caddy /var/log/ts-funnel \
 && chown -R tsfunnel:tsfunnel \
      /var/lib/tailscale /var/run/tailscale /etc/caddy /var/lib/caddy /var/log/ts-funnel

ENV XDG_DATA_HOME=/var/lib \
    XDG_CONFIG_HOME=/etc \
    HOME=/var/lib/caddy

COPY --chmod=755 entrypoint.sh /entrypoint.sh
USER tsfunnel

VOLUME ["/var/lib/tailscale", "/etc/caddy"]
# Container-network access only; EXPOSE does not publish a host port.
EXPOSE 9090

HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/ts-funnel", "healthcheck"]
ENTRYPOINT ["/entrypoint.sh"]

LABEL org.opencontainers.image.source="https://github.com/emaori/ts-funnel-service" \
      org.opencontainers.image.description="Docker image to expose a service using Tailscale Funnel" \
      org.opencontainers.image.licenses="MIT" \
      io.ts-funnel-service.tailscale.version="${TAILSCALE_VERSION}" \
      io.ts-funnel-service.caddy.version="${CADDY_VERSION}" \
      io.ts-funnel-service.alpine.version="${ALPINE_VERSION}"
