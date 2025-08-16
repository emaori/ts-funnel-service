# Tailscale Funnel Service (Docker + Caddy)

A lightweight Docker image that exposes a local container to the Internet using [Tailscale Funnel](https://tailscale.com/kb/1223/funnel).  
No need for a public IP or router port-forwarding. SSL certificates are automatically issued by Tailscale.

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
  --cap-add NET_ADMIN \
  --device /dev/net/tun \
  -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
  -e TAILSCALE_HOSTNAME="myService" \
  -e SERVICE_PORT=<port of the container to expose> \
  -e SERVICE_NAME=<name of the container to expose> \
  ghcr.io/emaori/ts-funnel-service:latest
```

### Docker Compose

```yaml
version: "3.9"

services:
  ts-funnel-myService:
    image: ghcr.io/emaori/ts-funnel-service:latest
    container_name: ts-funnel-myService
    restart: always
    hostname: myService
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun
    environment:
      TAILSCALE_AUTHKEY: "tskey-auth-<...>"
      TAILSCALE_HOSTNAME: "myService"
      SERVICE_PORT: "<port of the container to expose>"
      SERVICE_NAME: "<name of the container to expose>"
```

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
  --cap-add NET_ADMIN \
  --device /dev/net/tun \
  -e TAILSCALE_AUTHKEY="tskey-auth-<...>" \
  -e TAILSCALE_HOSTNAME='myService' \
  -e SERVICE_PORT=<port of the container to expose> \
  -e SERVICE_NAME=<name of the container to expose> \
  -e USE_CUSTOM_CADDYFILE=true \
  -v /opt/data/ts-funnel/Caddyfile:/etc/caddy/Caddyfile \
  ghcr.io/emaori/ts-funnel-service:latest
```

⚠️ **Note**: when using a custom Caddyfile, it is entirely your responsibility to provide a valid and working configuration.

### Origin not allowed

Some services (like Grafana) may reject requests from unknown domains, resulting in an **"origin not allowed"** error.  
To bypass this restriction, you can set the environment variable `ALLOW_ALL_ORIGIN` to `true`.

## Environment variables

| Name                   | Description                                                                 | Mandatory |
| ---------------------- | --------------------------------------------------------------------------- | --------- |
| `TAILSCALE_AUTHKEY`    | Tailscale authorization key                                                 | Yes       |
| `TAILSCALE_HOSTNAME`   | Hostname used to configure the Tailscale connection                         | Yes       |
| `SERVICE_PORT`         | Port of the local container to expose                                       | Yes, unless `USE_CUSTOM_CADDYFILE` is `true` |
| `SERVICE_NAME`         | Name (or IP address) of the local container to expose                       | Yes, unless `USE_CUSTOM_CADDYFILE` is `true` |
| `USE_CUSTOM_CADDYFILE` | Set to `true` to provide a custom Caddyfile (⚠️ advanced usage)             | No        |
| `ALLOW_ALL_ORIGIN`     | Set to `true` to bypass "origin not allowed" errors for some services        | No        |
