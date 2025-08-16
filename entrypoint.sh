#!/bin/bash
set -euo pipefail

log(){ echo "[$(date +'%Y-%m-%d %H:%M:%S')] $1"; }

cleanup(){
  log "Shutting down services..."
  [[ -n "${CADDY_PID:-}" ]] && kill "$CADDY_PID" 2>/dev/null || true
  [[ -n "${TAILSCALE_PID:-}" ]] && kill "$TAILSCALE_PID" 2>/dev/null || true
  tailscaled --cleanup 2>/dev/null || true
  exit 0
}
trap cleanup SIGTERM SIGINT

log "Starting Tailscale+Caddy container..."

if [[ -z "${TAILSCALE_AUTHKEY:-}" ]]; then
  log "ERROR: TAILSCALE_AUTHKEY environment variable is required"
  exit 125
fi
if [[ -z "${TAILSCALE_HOSTNAME:-}" ]]; then
  log "ERROR: TAILSCALE_HOSTNAME environment variable is required"
  exit 125
fi

# Ensure dirs exist
mkdir -p /var/lib/tailscale /var/run/tailscale

log "Starting tailscaled daemon..."
tailscaled --state=/var/lib/tailscale/tailscaled.state \
           --socket=/var/run/tailscale/tailscaled.sock &
TAILSCALE_PID=$!

# Wait for tailscaled socket to be available
log "Waiting for tailscaled socket to be available..."
for i in {1..30}; do
    if [ -S /var/run/tailscale/tailscaled.sock ]; then
        break
    fi
    if [ $i -eq 30 ]; then
        log "ERROR: tailscaled socket not available within 30 seconds"
        exit 125
    fi
    sleep 125
done
log "tailscaled socket is available"

log "Authenticating with Tailscale..."
UP_ARGS=(--authkey="$TAILSCALE_AUTHKEY" --accept-routes --accept-dns=false)
[[ -n "${TAILSCALE_HOSTNAME:-}" ]] && UP_ARGS+=(--hostname="$TAILSCALE_HOSTNAME")
tailscale up "${UP_ARGS[@]}"

log "Tailscale up successful"

tailscale serve --reset || true
tailscale funnel off || true

log "Tailscale reset and funnel off completed"

CADDY_CONFIG="/etc/caddy/Caddyfile"

# Check if user wants to use custom Caddyfile or generate default
if [[ "${USE_CUSTOM_CADDYFILE:-false}" == "true" ]]; then
    log "USE_CUSTOM_CADDYFILE=true, using existing Caddyfile at $CADDY_CONFIG"
    if [ ! -f "$CADDY_CONFIG" ]; then
        log "ERROR: USE_CUSTOM_CADDYFILE=true but no Caddyfile found at $CADDY_CONFIG"
        exit 125
    fi
else
    log "Generating default Caddyfile (set USE_CUSTOM_CADDYFILE=true to use custom config)..."
    
    if [[ -z "${SERVICE_NAME:-}" ]]; then
      log "ERROR: SERVICE_NAME environment variable is required"
      exit 125
    fi
    if [[ -z "${SERVICE_PORT:-}" ]]; then
      log "ERROR: SERVICE_PORT environment variable is required"
      exit 125
    fi

    # Check if CORS headers should be added
    CORS_HEADERS=""
    if [[ "${ALLOW_ALL_ORIGIN:-}" == "true" ]]; then
        log "ALLOW_ALL_ORIGIN is true, adding CORS headers"
        CORS_HEADERS="
        header_down Access-Control-Allow-Origin *
        header_down Access-Control-Allow-Credentials true"
    else
        log "ALLOW_ALL_ORIGIN is not set to true, skipping CORS headers"
    fi

    cat > "$CADDY_CONFIG" << EOF
{
    auto_https off
}

:8080 {
    reverse_proxy $SERVICE_NAME:$SERVICE_PORT {
        header_up Host {http.request.host}
        header_up X-Forwarded-Proto {http.request.scheme}
        header_up X-Forwarded-For {http.request.remote}$CORS_HEADERS
    }
}
EOF
    log "Default Caddyfile created at $CADDY_CONFIG for $SERVICE_NAME:$SERVICE_PORT"
fi

log "Starting Caddy..."
caddy run --config "$CADDY_CONFIG" --adapter caddyfile &
CADDY_PID=$!

sleep 2
if ! curl -fsS http://127.0.0.1:8080 >/dev/null 2>&1; then
  log "WARNING: Caddy may not be responding on port 8080"
else
  log "Caddy is running and responding on port 8080"
fi

# Configure Funnel directly to Caddy (public internet via *.ts.net)
# Note: only 127.0.0.1 proxies are supported by Funnel.
log "Enabling Tailscale Funnel on 443 to Caddy:8080..."
tailscale funnel --bg --https=443 --set-path=/ http://127.0.0.1:8080
log "Funnel enabled"

log "Setup complete!"

# Simple watchdog
while true; do
  if ! kill -0 "$TAILSCALE_PID" 2>/dev/null; then log "ERROR: tailscaled died"; exit 125; fi
  if ! kill -0 "$CADDY_PID" 2>/dev/null; then log "ERROR: Caddy died"; exit 125; fi
  if ! tailscale status >/dev/null 2>&1; then log "WARNING: Tailscale connectivity issue detected"; fi
  sleep 10
done
