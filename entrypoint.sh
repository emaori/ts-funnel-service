#!/bin/sh
#
# ts-funnel-service entrypoint (POSIX sh, busybox ash compatible)
# Starts tailscaled (userspace networking) + Caddy and exposes a local
# service via Tailscale Funnel.
#
set -eu

readonly TAILSCALE_SOCKET="/var/run/tailscale/tailscaled.sock"
readonly TAILSCALE_STATE="/var/lib/tailscale/tailscaled.state"
readonly CADDY_CONFIG="/etc/caddy/Caddyfile"
readonly CADDY_PORT=8080
readonly SOCKET_TIMEOUT_SECONDS=30
readonly CADDY_READY_TIMEOUT_SECONDS=10
# Liveness polling (cheap: shell builtins only). Overridable via env.
readonly WATCHDOG_INTERVAL_SECONDS="${WATCHDOG_INTERVAL_SECONDS:-30}"
# Tailscale connectivity check (expensive: forks the tailscale CLI and
# queries the daemon). Runs far less often. Set to 0 to disable.
readonly STATUS_CHECK_INTERVAL_SECONDS="${STATUS_CHECK_INTERVAL_SECONDS:-300}"

TAILSCALED_PID=""
CADDY_PID=""

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

fatal() {
    log "ERROR: $*"
    exit 1
}

cleanup() {
    log "Shutting down services..."
    if [ -n "$CADDY_PID" ] && kill -0 "$CADDY_PID" 2>/dev/null; then
        kill "$CADDY_PID" 2>/dev/null || true
        wait "$CADDY_PID" 2>/dev/null || true
    fi
    if [ -n "$TAILSCALED_PID" ] && kill -0 "$TAILSCALED_PID" 2>/dev/null; then
        kill "$TAILSCALED_PID" 2>/dev/null || true
        wait "$TAILSCALED_PID" 2>/dev/null || true
    fi
    # Note: no `tailscaled --cleanup` here: it only removes kernel-mode
    # iptables rules and routes, which don't exist in userspace mode.
    log "Shutdown complete"
    exit 0
}
trap cleanup TERM INT

require_env() {
    name="$1"
    eval "value=\${$name:-}"
    [ -n "$value" ] || fatal "$name environment variable is required"
}

start_tailscaled() {
    log "Starting tailscaled daemon (userspace networking)..."
    mkdir -p /var/lib/tailscale /var/run/tailscale

    tailscaled \
        --state="$TAILSCALE_STATE" \
        --socket="$TAILSCALE_SOCKET" \
        --tun=userspace-networking &
    TAILSCALED_PID=$!

    log "Waiting for tailscaled socket (timeout: ${SOCKET_TIMEOUT_SECONDS}s)..."
    i=1
    while [ "$i" -le "$SOCKET_TIMEOUT_SECONDS" ]; do
        if [ -S "$TAILSCALE_SOCKET" ]; then
            log "tailscaled socket is available"
            return 0
        fi
        kill -0 "$TAILSCALED_PID" 2>/dev/null \
            || fatal "tailscaled exited unexpectedly during startup"
        sleep 1
        i=$((i + 1))
    done
    fatal "tailscaled socket not available within ${SOCKET_TIMEOUT_SECONDS} seconds"
}

tailscale_up() {
    log "Authenticating with Tailscale..."
    tailscale up \
        --authkey="$TAILSCALE_AUTHKEY" \
        --hostname="$TAILSCALE_HOSTNAME" \
        --accept-dns=false
    log "Tailscale up successful"

    # Start from a clean serve/funnel configuration
    tailscale serve --reset 2>/dev/null || true
    tailscale funnel off 2>/dev/null || true
    log "Tailscale serve reset and funnel off completed"
}

generate_caddyfile() {
    require_env SERVICE_NAME
    require_env SERVICE_PORT

    case "$SERVICE_PORT" in
        '' | *[!0-9]*) fatal "SERVICE_PORT must be numeric, got: '$SERVICE_PORT'" ;;
    esac
    if [ "$SERVICE_PORT" -lt 1 ] || [ "$SERVICE_PORT" -gt 65535 ]; then
        fatal "SERVICE_PORT must be a valid TCP port (1-65535), got: '$SERVICE_PORT'"
    fi

    cors_headers=""
    if [ "${ALLOW_ALL_ORIGIN:-false}" = "true" ]; then
        log "ALLOW_ALL_ORIGIN=true, adding CORS headers"
        cors_headers="
        header_down Access-Control-Allow-Origin *
        header_down Access-Control-Allow-Credentials true"
    fi

    cat > "$CADDY_CONFIG" << EOF
{
    auto_https off
}

:${CADDY_PORT} {
    reverse_proxy ${SERVICE_NAME}:${SERVICE_PORT} {
        header_up Host {http.request.host}
        header_up X-Forwarded-Proto {http.request.scheme}
        header_up X-Forwarded-For {http.request.remote}${cors_headers}
    }
}
EOF
    log "Default Caddyfile created at $CADDY_CONFIG for ${SERVICE_NAME}:${SERVICE_PORT}"
}

prepare_caddy_config() {
    if [ "${USE_CUSTOM_CADDYFILE:-false}" = "true" ]; then
        log "USE_CUSTOM_CADDYFILE=true, using existing Caddyfile at $CADDY_CONFIG"
        [ -f "$CADDY_CONFIG" ] \
            || fatal "USE_CUSTOM_CADDYFILE=true but no Caddyfile found at $CADDY_CONFIG"
    else
        log "Generating default Caddyfile (set USE_CUSTOM_CADDYFILE=true to use a custom config)..."
        generate_caddyfile
    fi

    log "Validating Caddy configuration..."
    caddy validate --config "$CADDY_CONFIG" --adapter caddyfile \
        || fatal "Caddyfile validation failed"
}

start_caddy() {
    log "Starting Caddy..."
    caddy run --config "$CADDY_CONFIG" --adapter caddyfile &
    CADDY_PID=$!

    # Wait until Caddy accepts TCP connections on its port. A successful
    # connect means Caddy itself is up, regardless of upstream health.
    i=1
    while [ "$i" -le "$CADDY_READY_TIMEOUT_SECONDS" ]; do
        if nc -z 127.0.0.1 "$CADDY_PORT" 2>/dev/null; then
            log "Caddy is running and listening on port ${CADDY_PORT}"
            return 0
        fi
        kill -0 "$CADDY_PID" 2>/dev/null \
            || fatal "Caddy exited unexpectedly during startup"
        sleep 1
        i=$((i + 1))
    done
    log "WARNING: Caddy not listening on port ${CADDY_PORT} after ${CADDY_READY_TIMEOUT_SECONDS}s, continuing anyway"
}

enable_funnel() {
    # Funnel only supports proxying to 127.0.0.1
    log "Enabling Tailscale Funnel on 443 -> 127.0.0.1:${CADDY_PORT}..."
    tailscale funnel --bg --https=443 --set-path=/ "http://127.0.0.1:${CADDY_PORT}"
    log "Funnel enabled"
}

watchdog() {
    log "Setup complete!"

    # Stagger the cycle across containers started at the same time, so that
    # N containers on the same host don't all wake up in the same instant.
    stagger=$(( $$ % WATCHDOG_INTERVAL_SECONDS ))
    if [ "$stagger" -gt 0 ]; then
        sleep "$stagger" &
        wait $! || true
    fi

    since_status_check=0
    while true; do
        # Cheap liveness checks: kill -0 is a shell builtin, no fork.
        kill -0 "$TAILSCALED_PID" 2>/dev/null || fatal "tailscaled died"
        kill -0 "$CADDY_PID" 2>/dev/null || fatal "Caddy died"

        # Expensive connectivity check (forks the tailscale CLI): only
        # every STATUS_CHECK_INTERVAL_SECONDS, and only if enabled.
        if [ "$STATUS_CHECK_INTERVAL_SECONDS" -gt 0 ] \
            && [ "$since_status_check" -ge "$STATUS_CHECK_INTERVAL_SECONDS" ]; then
            tailscale status >/dev/null 2>&1 \
                || log "WARNING: Tailscale connectivity issue detected"
            since_status_check=0
        fi

        # Sleep in background + wait so SIGTERM is handled immediately
        sleep "$WATCHDOG_INTERVAL_SECONDS" &
        wait $! || true
        since_status_check=$((since_status_check + WATCHDOG_INTERVAL_SECONDS))
    done
}

main() {
    log "Starting Tailscale+Caddy container..."
    require_env TAILSCALE_AUTHKEY
    require_env TAILSCALE_HOSTNAME

    start_tailscaled
    tailscale_up
    prepare_caddy_config
    start_caddy
    enable_funnel
    watchdog
}

main "$@"
