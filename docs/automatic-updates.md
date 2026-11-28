# Automatic Tailscale updates

Automatic updates are enabled by default. Set `TAILSCALE_AUTO_UPDATE_ENABLED=false` when creating the container to opt out. Recreate the container to change its environment. Disabled mode serves the last committed runtime version without scheduled release requests, downloads or downgrades. Recovery, supervision, logs and metrics remain enabled.

This updates Tailscale inside the existing unprivileged container. Caddy, Alpine and the supervisor remain at their image versions. Caddy updates and dependency-triggered image publishing are deferred.

## Switch and rollback

A and B are reusable slots in the persistent `/var/lib/tailscale` volume:

| Point | A | B |
| --- | --- | --- |
| Initial start | Image seed, active | Empty |
| First accepted update | Outgoing rollback copy | New active version |
| Second accepted update | New active version | Outgoing rollback copy |

The Go supervisor owns process lifecycle and serializes updates under a lifetime volume lock. Two production daemons never run concurrently with the same node identity.

1. Read official stable Linux tarball metadata and select a newer release for the running architecture. Never automatically downgrade or select unstable releases.
2. Record an attempt and capture the node ID, DNS name, local Caddy/Funnel configuration and a public HTTPS baseline.
3. Download to scratch storage while the outgoing daemon serves traffic. Authenticate the signing-key bundle against embedded upstream roots, then verify the archive signature with upstream `distsign` primitives. Reject unsafe archives, wrong architecture, mismatched CLI/daemon versions or missing binaries. Persist artifact provenance and executable SHA-256 hashes.
4. Start an isolated userspace preflight daemon with empty temporary state and a separate socket. Check its LocalAPI version and unauthenticated state, then confirm it exited. Preflight receives neither the production auth key nor its state.
5. Recheck the outgoing baseline. Only then replace the inactive slot's binaries. Its older rollback copy is no longer available once this slot is reused.
6. Journal the disruptive boundary, send SIGTERM to the outgoing daemon, and confirm exit within 10 seconds. Stop timeout prevents starting the candidate.
7. Copy the quiescent daemon-managed state into the candidate: identity, preferences, Serve/Funnel settings, certificates, Tailnet Lock material and other regular state files. Omit transient sockets and reject symlinks/special files. Leave the outgoing directory untouched.
8. Start the candidate with the copied state. Check its observed version, original node ID/DNS name, expected background Funnel forwarding and three consecutive matching public HTTPS responses.
9. Atomically commit the binary/state pair, result, timestamps and counters together, using file/directory syncing. Retain the outgoing pair until the next update reuses its slot.

Public access is interrupted during steps 6–8 because Funnel terminates in `tailscaled`. Caddy and the independent metrics server stay running. Existing TCP/WebSocket sessions may be lost. Configure a stop grace period of about two minutes (Compose `stop_grace_period: 2m`) to allow bounded shutdown/rollback; a forced kill is recovered from the journal on restart. The interruption metric estimates the stop-boundary-to-validation period, including probe time; it is not a measured client-outage guarantee.

If copying, startup or validation fails, confirm the candidate stopped, restart the outgoing executable with its original state, and validate recovery. Record `rolled_back`, never success. Quarantine the failed release to prevent repeated outages; a newer candidate can be considered later.

If restoration fails, record `rollback_failed` and halt further updates. Caddy, metrics and logs stay available and Docker health becomes unhealthy. Docker does not restart a merely unhealthy container automatically. Correct connectivity/credentials/storage and restart the container to recover the committed slot. Do not delete identity state to solve transient failures.

## Schedule and public validation

The default check interval is six hours with independently generated ±10% random jitter. The first check follows startup by about two minutes. Persist the next check across restart; spread overdue checks. Metadata and non-disruptive preparation failures back off from 15 minutes, doubling up to the configured interval. No-update checks do not advance install counters or successful-update time.

Public probes use HTTPS with certificate verification, no HTTP proxy and only public DNS destinations. They reject private, MagicDNS and Tailscale addresses and do not follow redirects. A stable HTTP status from 200 through 499 is a valid baseline, including authentication responses and redirects; 5xx fails validation. Status codes, rather than page contents, are compared.

If the system resolver returns private tailnet addresses for the `.ts.net` name, configure `TAILSCALE_UPDATE_PROBE_DNS_ADDRESS` with a reachable public-view resolver, e.g. `1.1.1.1:53`. No external resolver is hardcoded. Choose a safe stable GET path with `TAILSCALE_UPDATE_PROBE_PATH` (default `/`), especially with custom Caddy configurations. Avoid paths with side effects or varying response statuses. An unhealthy baseline defers handover.

## Persistence, migration and recovery

Keep a distinct named state volume per service. Simultaneous supervisor use of a volume is refused.

```text
/var/lib/tailscale/
  tailscaled.state              # Original legacy state retained on migration
  updater/
    active-record              # Committed slot and summaries
    transaction-record         # Present during a transaction
    bootstrap-started
    bootstrap-complete
    update.lock
    history/                   # Last 100 private attempt records
    logs/updates.log
    staging/                   # Uncommitted scratch, cleaned on startup
    slots/{a,b}/
      bin/{tailscale,tailscaled,artifact-record}
      state/
```

On first start, copy flat legacy state to A before starting a production daemon. Original flat files remain intact. Later starts use the committed slot, including after image replacement or opting out of updates. A persisted slot takes precedence over the baked-in seed.

Back up the full volume before migration. Older entrypoints read the now-stale flat files: restore a matching full backup when reverting to an older image. Do not mix entrypoints/layouts.

A fresh node needs `TAILSCALE_AUTHKEY`. After identity is established, restarts and handovers do not reuse it. An expired persisted identity is not silently replaced with a new enrollment. Mounts must be writable by the configured UID/GID, normally 1000.

After interruption, recover the committed pair rather than an uncommitted candidate. A durably accepted candidate wins if only cleanup was interrupted. Outcomes count once. Corrupt/missing initialized metadata, changed binaries and identity mismatches fail closed instead of reseeding.

State isolation cannot undo coordination-server changes or guarantee compatibility across arbitrary Tailscale versions, expired credentials or external outages. See the [validation plan](testing.md) for the authenticated release gate and current limits.

## Logs, mounts and retry

```bash
docker logs --since 1h ts-funnel-myService
docker exec ts-funnel-myService tail -n 100 /var/lib/tailscale/updater/logs/updates.log
docker exec ts-funnel-myService /usr/local/bin/ts-funnel retry-update
```

Events go to stdout and human-readable `updates.log`. Lines include UTC time, phase/result attempt IDs, versions and diagnostics, with auth-key redaction. Caddy keeps its ordinary stdout logging; the update file contains supervisor/updater/Tailscale events. Full state/credential files are not logged. Error prose remains in logs, not metric labels.

The local retry command queues only the quarantined version and is checked on the watchdog interval. The outgoing service must be locally ready; the enable flag still applies. There is no network API. A scheduler stopped by a persistence error requires fixing storage and restarting the container.

Logs rotate at five MiB, retaining five total files by default (`updates.log` plus `.1`–`.4`). Entries are bounded. Docker logging-driver rotation is separate. File errors are reported on stdout and defer new attempts; restoration already underway continues.

To expose a separate host log folder:

```yaml
services:
  funnel:
    environment:
      UPDATE_LOG_DIR: /var/log/ts-funnel
    volumes:
      - funnel-state:/var/lib/tailscale
      - /opt/ts-funnel/logs:/var/log/ts-funnel
```

Create the host directory and give UID/GID 1000 write access. A separate named volume, e.g. `funnel-logs:/var/log/ts-funnel`, also works; declare it under top-level `volumes`. Keep journals in the state volume. Do not share log directories between containers or point them at a slot.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `TAILSCALE_AUTO_UPDATE_ENABLED` | `true` | Scheduled stable updates; explicit `false` opts out. |
| `TAILSCALE_UPDATE_CHECK_INTERVAL_SECONDS` | `21600` | Positive interval in seconds, with jitter. |
| `TAILSCALE_UPDATE_VALIDATION_TIMEOUT_SECONDS` | `60` | Candidate/public validation budget; rollback has a separate bounded recovery budget. |
| `TAILSCALE_UPDATE_PROBE_DNS_ADDRESS` | System resolver | Optional public-view DNS host:port. |
| `TAILSCALE_UPDATE_PROBE_PATH` | `/` | Safe stable GET path, no query/fragment. |
| `METRICS_ENABLED` | `true` | Independent monitoring endpoint. |
| `METRICS_LISTEN_ADDRESS` | `0.0.0.0:9090` | Container-network monitoring address. |
| `UPDATE_LOG_DIR` | `/var/lib/tailscale/updater/logs` | Absolute file-log directory. |
| `UPDATE_LOG_MAX_SIZE_BYTES` | `5242880` | Per-file cap, 1024–1073741824 bytes. |
| `UPDATE_LOG_MAX_FILES` | `5` | Total file count, 1–100. |

Booleans accept exactly `true`/`false`. Malformed settings fail startup. The native Tailscale updater is disabled in both modes so only this supervisor owns replacement.

## Implementation boundaries

`internal/updater` owns scheduling, locking, persistence, slots and outcomes. Its `Driver` supplies distribution/process validation; `internal/tailscale` is the only implementation. `internal/observability` keeps scraping independent of child processes and updates. A future Caddy driver can reuse the transaction machinery after defining its own state/compatibility policy.

Linux amd64/arm64 are supported build targets. Go is built statically in a separate stage; the runtime stays unprivileged userspace Alpine without a compiler, Docker socket, TUN or NET_ADMIN. Embedded signature roots carry upstream attribution; root trust changes require an image update.
