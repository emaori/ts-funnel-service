# syntax=docker/dockerfile:1

# Pinned versions (current stable as of 2026-06)
# Note: the tailscale/tailscale Docker image lags behind the GitHub client
# releases. Pin to the newest *image* tag (same digest as :stable), not the
# newest git tag. Check: https://hub.docker.com/r/tailscale/tailscale/tags
ARG ALPINE_VERSION=3.24.1
ARG TAILSCALE_VERSION=v1.98.4
ARG CADDY_VERSION=2.11.4

# --- Source stages: official images, used only to copy the static binaries ---
FROM tailscale/tailscale:${TAILSCALE_VERSION} AS tailscale
FROM caddy:${CADDY_VERSION}-alpine AS caddy

# --- Final image ---
# No apk packages needed: the entrypoint is POSIX sh (busybox ash), readiness
# checks use busybox nc, and the TLS trust store (ca-certificates-bundle)
# ships with the Alpine base image.
#
# Rootless: tailscaled runs in userspace-networking mode (no TUN device, no
# netfilter), so the whole stack can run as an unprivileged user. No
# NET_ADMIN capability or /dev/net/tun device is required at runtime.
FROM alpine:${ALPINE_VERSION}

ARG TAILSCALE_VERSION
ARG CADDY_VERSION
ARG ALPINE_VERSION
ARG UID=1000
ARG GID=1000

COPY --from=tailscale /usr/local/bin/tailscale  /usr/local/bin/tailscale
COPY --from=tailscale /usr/local/bin/tailscaled /usr/local/bin/tailscaled
COPY --from=caddy     /usr/bin/caddy            /usr/local/bin/caddy

# Non-root user and writable runtime directories:
# - /var/lib/tailscale  tailscaled state (persist via volume)
# - /var/run/tailscale  tailscaled LocalAPI socket
# - /etc/caddy          generated or user-provided Caddyfile
# - /var/lib/caddy      Caddy storage (via XDG_DATA_HOME)
RUN addgroup -g "${GID}" tsfunnel \
 && adduser -D -H -u "${UID}" -G tsfunnel tsfunnel \
 && mkdir -p /var/lib/tailscale /var/run/tailscale /etc/caddy /var/lib/caddy \
 && chown -R tsfunnel:tsfunnel \
        /var/lib/tailscale /var/run/tailscale /etc/caddy /var/lib/caddy

# Caddy resolves its storage and config dirs from XDG variables:
# XDG_DATA_HOME=/var/lib -> /var/lib/caddy, XDG_CONFIG_HOME=/etc -> /etc/caddy
ENV XDG_DATA_HOME=/var/lib \
    XDG_CONFIG_HOME=/etc \
    HOME=/var/lib/caddy

COPY --chmod=755 entrypoint.sh /entrypoint.sh

USER tsfunnel

VOLUME ["/var/lib/tailscale", "/etc/caddy"]

# Cheap healthcheck: a TCP connect to Caddy (busybox nc, no Go binary exec).
# Tailscale connectivity is monitored by the entrypoint watchdog instead.
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
    CMD nc -z 127.0.0.1 8080 || exit 1

ENTRYPOINT ["/entrypoint.sh"]

LABEL org.opencontainers.image.source="https://github.com/emaori/ts-funnel-service" \
      org.opencontainers.image.description="Docker image to expose a service using Tailscale Funnel" \
      org.opencontainers.image.licenses="MIT" \
      io.ts-funnel-service.tailscale.version="${TAILSCALE_VERSION}" \
      io.ts-funnel-service.caddy.version="${CADDY_VERSION}" \
      io.ts-funnel-service.alpine.version="${ALPINE_VERSION}"
