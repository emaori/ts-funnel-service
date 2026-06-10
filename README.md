# Tailscale Funnel Service (Tailscale + Caddy)

A lightweight Docker image that exposes a local container to the Internet using [Tailscale Funnel](https://tailscale.com/kb/1223/funnel).  
No need for a public IP or router port-forwarding. SSL certificates are automatically issued by Tailscale.

The image is **rootless** and runs Tailscale in [userspace networking](https://tailscale.com/kb/1112/userspace-networking) mode: no `NET_ADMIN` capability, no `/dev/net/tun` device, and no root user are required. It also works out of the box on rootless Docker and Podman.

## Pull the image

```bash
# Get the latest version
docker pull ghcr.io/emaori/ts-funnel-service:latest

# Or pull a specific version
docker pull ghcr.io/emaori/ts-funnel-service:1.0.0
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
```

> **Upgrading from a previous version?**
> - `--cap-add NET_ADMIN` and `--device /dev/net/tun` are no longer needed and can be removed from your configuration.
> - The container now runs as the unprivileged user `tsfunnel` (UID/GID `1000`). If you reuse a volume or bind mount created by a previous (root-based) version of this image, fix its ownership once on the host:
>   ```bash
>   chown -R 1000:1000 <path of the mounted directory>
>   ```

## Persisting the Tailscale identity

To keep the same Tailscale node identity (and IP) across container re-creations, mount a volume on `/var/lib/tailscale`:

```yaml
    volumes:
      - ts-funnel-state:/var/lib/tailscale
```

Or with `docker run`:

```bash
docker run -d \
  --name ts-funnel-myService \
  --restart=always \
  --hostname myService \
  -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
  -e TAILSCALE_HOSTNAME="myService" \
  -e SERVICE_PORT=<port of the container to expose> \
  -e SERVICE_NAME=<name of the container to expose> \
  -v ts-funnel-state:/var/lib/tailscale \
  ghcr.io/emaori/ts-funnel-service:latest
```

Docker creates the named volume automatically on first start. If you run multiple `ts-funnel-service` containers on the same host, use a distinct volume name for each one (e.g. `ts-funnel-grafana-state`, `ts-funnel-blog-state`): every container is a separate Tailscale node with its own identity.

With a **named volume** (recommended) no extra step is needed: Docker copies the correct ownership from the image. With a **bind mount**, the host directory must be writable by UID `1000`:

```bash
mkdir -p /opt/data/ts-funnel/state
chown 1000:1000 /opt/data/ts-funnel/state
```

Alternatively, rebuild the image with `--build-arg UID=$(id -u) --build-arg GID=$(id -g)` to match your host user.

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
