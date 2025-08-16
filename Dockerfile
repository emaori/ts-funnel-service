FROM alpine:latest
RUN apk add --no-cache caddy curl iptables bash libc6-compat ip6tables tailscale
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
VOLUME ["/var/lib/tailscale", "/data", "/etc/caddy"]
ENTRYPOINT ["/entrypoint.sh"]

LABEL org.opencontainers.image.source https://github.com/emaori/ts-funnel-service
LABEL org.opencontainers.image.description "Docker image to expose a service using Tailscale Funnel"
LABEL org.opencontainers.image.licenses MIT
