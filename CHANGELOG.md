# Changelog

## Unreleased

- Enable in-container stable Tailscale updates by default; opt out with `TAILSCALE_AUTO_UPDATE_ENABLED=false` at creation.
- Add official signature verification, isolated preflight, alternating binary/state slots, bounded rollback and crash recovery. Brief Funnel interruptions and dropped existing connections are possible.
- Add OpenMetrics, human-readable rotating logs, configurable log mounts, local quarantine retry and one reusable multi-service Grafana dashboard.
- Migrate legacy state into slots in the existing volume. Committed runtime versions take precedence over image seeds. Back up before migration; older entrypoints use stale flat state.
- Replace shell lifecycle management with Go while retaining userspace networking and generated/custom Caddy support.
- Caddy/Alpine updates and dependency-triggered image publishing remain deferred. See [rollout validation limits](docs/testing.md).
