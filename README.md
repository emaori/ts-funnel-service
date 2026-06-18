# Tailscale Funnel Service (Tailscale + Caddy)
[![Docker build](https://github.com/emaori/ts-funnel-service/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/emaori/ts-funnel-service/actions/workflows/docker-publish.yml)
[![Latest release](https://img.shields.io/github/v/release/emaori/ts-funnel-service?sort=semver)](https://github.com/emaori/ts-funnel-service/releases)
[![License: MIT](https://img.shields.io/github/license/emaori/ts-funnel-service)](LICENSE)
[![ghcr.io](https://img.shields.io/badge/ghcr.io-ts--funnel--service-blue?logo=docker)](https://github.com/emaori/ts-funnel-service/pkgs/container/ts-funnel-service)

A lightweight Docker image that exposes a local container to the Internet using [Tailscale Funnel](https://tailscale.com/kb/1223/funnel).  
No need for a public IP or router port-forwarding. SSL certificates are automatically issued by Tailscale.

The image is **rootless** and runs Tailscale in [userspace networking](https://tailscale.com/kb/1112/userspace-networking) mode: no `NET_ADMIN` capability, no `/dev/net/tun` device, and no root user are required. It also works out of the box on rootless Docker and Podman.

## Pull the image

```bash
# Get the latest version
docker pull ghcr.io/emaori/ts-funnel-service:latest

# Or pull a specific version
docker pull ghcr.io/emaori/ts-funnel-service:2.0.0
```

## Basic usage

1. Sign up for free at [tailscale.com](https://tailscale.com/).
2. In the Tailscale dashboard, create a new authentication key:
   1. Go to **Machines**
   2. Click **Add device**
   3. Add a tag to disable key expiry
   4. Generate the install script
   5. Copy the key from the script
3. Create a container using `docker run` or Docker Compose (see below).
4. Funnel is disabled by default. Check the container logs for the activation link.
5. Access your service using your Tailscale domain:  
   `https://<host_name>.<tailscale_domain>.ts.net`
6. On the first request, it may take some time for the SSL certificate to be issued. Refresh the page and it should work with the SSL certificate.

The examples below mount a named volume on `/var/lib/tailscale`. This is what keeps your Tailscale node identity (and IP) stable across container re-creations, and it is **strongly recommended** — see [Persisting the Tailscale identity](#persisting-the-tailscale-identity) for the details.

### Docker run command

```bash
docker run -d \
  --name ts-funnel-myService \
  --restart=always \
  --hostname myService \
  -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
  -e TAILSCALE_HOSTNAME="myService" \
  -e SERVICE_PORT=<port of the container to expose> \
  -e SERVICE_NAME=<name of the container to expose> \
  -v ts-funnel-myservice-state:/var/lib/tailscale \
  ghcr.io/emaori/ts-funnel-service:latest
```

### Docker Compose

```yaml
services:
  ts-funnel-myService:
    image: ghcr.io/emaori/ts-funnel-service:latest
    container_name: ts-funnel-myService
    restart: always
    hostname: myService
    environment:
      TAILSCALE_AUTHKEY: "tskey-auth-<...>"
      TAILSCALE_HOSTNAME: "myService"
      SERVICE_PORT: "<port of the container to expose>"
      SERVICE_NAME: "<name of the container to expose>"
    volumes:
      - ts-funnel-myservice-state:/var/lib/tailscale

volumes:
  ts-funnel-myservice-state:
```

Docker creates the named volume automatically on first start; you don't need to create it beforehand.

> Upgrading from a 1.x version? See [Migrating from v1.x](#migrating-from-v100) below.

## Migrating from v1.0.0

Version 2.0.0 makes the image **rootless** and switches Tailscale to **userspace networking**. The functionality is the same, but a few things change in how you run the container.

### 1. Remove capabilities and devices (recommended)

`--cap-add NET_ADMIN` and `--device /dev/net/tun` are no longer needed. Remove them from your `docker run` commands and Compose files:

```diff
   docker run -d \
     --name ts-funnel-myService \
     --restart=always \
     --hostname myService \
-    --cap-add NET_ADMIN \
-    --device /dev/net/tun \
     -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
     ...
```

The container will still start if you leave them in place, but they grant privileges that are no longer used.

### 2. Fix ownership of existing volumes (only if you persisted state in v1)

**This step applies only if your 1.x container mounted a volume on `/var/lib/tailscale` to persist its state.** If you never mounted a volume, skip it: the new container starts from a clean state and simply re-authenticates.

The reason is the change of user, not the volume itself. Version 1.x ran the container as **root** (UID `0`), so every file written to that volume is owned by root. Version 2.0.0 runs as the unprivileged user `tsfunnel` (UID/GID `1000`), which has no permission to write over root-owned files — so `tailscaled` fails to open its state file at startup.

Fix the ownership once on the host. **Run only the command that matches the kind of volume you used in v1** (not both):

```bash
# If you used a BIND MOUNT, chown the host directory directly:
chown -R 1000:1000 /opt/data/ts-funnel

# If you used a NAMED VOLUME, chown its content through a temporary container
# (a named volume has no directly accessible host path):
docker run --rm -v ts-funnel-myservice-state:/data alpine chown -R 1000:1000 /data
```

Both commands do the same thing — set the ownership of the persisted state to UID/GID `1000` — they only differ because a bind mount is a real host path while a named volume lives inside Docker's storage area and must be reached from inside a container.

Alternatively, start fresh with a new volume: the container will re-authenticate and appear as a new node in your Tailscale dashboard (the old node can be deleted from there).

### 3. Custom Caddyfile must be readable by UID 1000 (if applicable)

If you mount a custom Caddyfile, make sure the file is readable by UID `1000` (e.g. `chmod 644`). A root-owned file with `600` permissions worked with 1.x but will fail now. Mounting it read-only (`:ro`) is recommended.

### 4. Behavior changes to be aware of

- The Caddy configuration is now validated at startup with `caddy validate`. An invalid custom Caddyfile makes the container exit immediately with a clear error, instead of starting with a silently broken proxy.
- `--accept-routes` is no longer passed to `tailscale up`: this container only needs inbound Funnel traffic and outbound Docker-network traffic, so accepting tailnet routes was unnecessary (and is not usable in userspace networking mode anyway).
- Two new optional environment variables control monitoring frequency: see [Monitoring intervals](#monitoring-intervals).

Nothing changes in how the target service is reached: as before, the `ts-funnel-service` container and the target container must share a user-defined Docker network so that `SERVICE_NAME` can be resolved (see [Custom network](#custom-network)).

## Persisting the Tailscale identity

The `docker run` and Compose examples above already mount a **named volume** (`ts-funnel-myservice-state`) on `/var/lib/tailscale`. That mount is what keeps the same Tailscale node identity, and the same IP, every time the container is re-created. This section explains why a *named* volume specifically is needed.

The image declares `/var/lib/tailscale` as a `VOLUME`, so even if you pass no `-v` flag Docker still backs that path with a volume. The catch is that this auto-created volume is **anonymous**: it gets a random hash name and is not reliably reattached when the container is re-created. As soon as you recreate the container (e.g. `docker rm` + `docker run`, or pulling a new image), a fresh empty volume is used, Tailscale generates a **brand-new identity**, and a new node — with a new IP — shows up in your dashboard.

A **named volume** avoids this: it has a stable name that you control, and Docker reattaches the very same volume to the recreated container, so the stored Tailscale state — and therefore the node identity — survives. This is why all the examples use a named volume rather than relying on the automatic one.

If you run multiple `ts-funnel-service` containers on the same host, give each one a **distinct** volume name (e.g. `ts-funnel-grafana-state`, `ts-funnel-blog-state`): every container is a separate Tailscale node with its own identity, so they must not share the same state volume.

With a **named volume** (recommended) no extra step is needed: Docker copies the correct ownership from the image. If you prefer a **bind mount**, the host directory must be writable by UID `1000`:

```bash
mkdir -p /opt/data/ts-funnel/state
chown 1000:1000 /opt/data/ts-funnel/state
```

Then mount it in place of the named volume, e.g. `-v /opt/data/ts-funnel/state:/var/lib/tailscale`. Alternatively, rebuild the image with `--build-arg UID=$(id -u) --build-arg GID=$(id -g)` to match your host user.

## Advanced usage

### Custom network

You can create a dedicated Docker network only for the service container and the `ts-funnel-service` container:

1. Create a new Docker network:  
   ```bash
   docker network create funnel-net
   ```
2. Attach the target container to the new network:  
   ```bash
   docker network connect funnel-net <container name>
   ```
3. Specify the new network in your `docker run` or Docker Compose configuration.

### Custom Caddyfile

The `ts-funnel-service` container internally uses [Caddy](https://caddyserver.com/) to route incoming Funnel requests to the target container.  
By default, it automatically generates a Caddyfile like this:

```bash
{
    auto_https off
}

:8080 {
    reverse_proxy $SERVICE_NAME:$SERVICE_PORT {
        header_up Host {http.request.host}
        header_up X-Forwarded-Proto {http.request.scheme}
        header_up X-Forwarded-For {http.request.remote}
    }
}
```

To provide a custom Caddyfile, set the environment variable `USE_CUSTOM_CADDYFILE` to `true` and mount your own file:

```bash
docker run -d \
  --name ts-funnel-myService \
  --restart=always \
  --hostname myService \
  -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
  -e TAILSCALE_HOSTNAME='myService' \
  -e USE_CUSTOM_CADDYFILE=true \
  -v ts-funnel-myservice-state:/var/lib/tailscale \
  -v /opt/data/ts-funnel/Caddyfile:/etc/caddy/Caddyfile:ro \
  ghcr.io/emaori/ts-funnel-service:latest
```

⚠️ **Note**: when using a custom Caddyfile, it is entirely your responsibility to provide a valid and working configuration. The configuration is validated with `caddy validate` at startup, and the container exits with a clear error if it is invalid. Since the container runs as UID `1000`, the mounted file must be readable by that user (e.g. `chmod 644`).

### Origin not allowed

Some services (like Grafana) may reject requests from unknown domains, resulting in an **"origin not allowed"** error.  
To bypass this restriction, you can set the environment variable `ALLOW_ALL_ORIGIN` to `true`.

### Monitoring intervals

The entrypoint includes a lightweight watchdog: it checks every `WATCHDOG_INTERVAL_SECONDS` that both `tailscaled` and Caddy are alive (a free, fork-less check) and exits if either died, so that Docker can restart the container. Much less frequently, every `STATUS_CHECK_INTERVAL_SECONDS`, it also verifies Tailscale connectivity and logs a warning if it is degraded. Cycles are automatically staggered across containers, so many instances on the same host don't wake up at the same time. The defaults are fine for most setups; tune them only if you need faster failure detection or even lower idle activity.

## Environment variables

| Name                            | Description                                                                  | Mandatory | Default |
| ------------------------------- | ---------------------------------------------------------------------------- | --------- | ------- |
| `TAILSCALE_AUTHKEY`             | Tailscale authorization key                                                  | Yes       | —       |
| `TAILSCALE_HOSTNAME`            | Hostname used to configure the Tailscale connection                          | Yes       | —       |
| `SERVICE_PORT`                  | Port of the local container to expose (1-65535)                              | Yes, unless `USE_CUSTOM_CADDYFILE` is `true` | — |
| `SERVICE_NAME`                  | Name (or IP address) of the local container to expose                        | Yes, unless `USE_CUSTOM_CADDYFILE` is `true` | — |
| `USE_CUSTOM_CADDYFILE`          | Set to `true` to provide a custom Caddyfile (⚠️ advanced usage)              | No        | `false` |
| `ALLOW_ALL_ORIGIN`              | Set to `true` to bypass "origin not allowed" errors for some services         | No        | `false` |
| `WATCHDOG_INTERVAL_SECONDS`     | How often the watchdog checks that `tailscaled` and Caddy are alive          | No        | `30`    |
| `STATUS_CHECK_INTERVAL_SECONDS` | How often Tailscale connectivity is verified (logs a warning); `0` disables it | No        | `300`   |

## Build arguments

The image pins all its dependencies. Versions can be overridden at build time:

| Name                | Description                                                        | Default   |
| ------------------- | ------------------------------------------------------------------ | --------- |
| `ALPINE_VERSION`    | Alpine base image version                                          | `3.23.4`  |
| `TAILSCALE_VERSION` | Tag of the official `tailscale/tailscale` image to copy binaries from | `v1.94.2` |
| `CADDY_VERSION`     | Tag of the official `caddy` image to copy the binary from          | `2.11.4`  |
| `UID` / `GID`       | UID/GID of the unprivileged `tsfunnel` user                        | `1000`    |
