# Security Policy

## Supported versions

Only the latest released version is supported. Older tags do not receive fixes.

## Reporting a vulnerability

Please report vulnerabilities privately via
[GitHub Security Advisories](https://github.com/emaori/ts-funnel-service/security/advisories/new).
Do not open a public issue for security problems.

You can expect an initial response within a week. This is a personal
open-source project, maintained on a best-effort basis.

## Image verification

Published images are signed with [cosign](https://github.com/sigstore/cosign)
(keyless, via GitHub Actions OIDC). Verify with:

```bash
cosign verify \
  --certificate-identity-regexp "https://github.com/emaori/ts-funnel-service" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/emaori/ts-funnel-service:latest
```

## Scope notes

This image exposes a local service to the public Internet by design.
Securing the *exposed service itself* (authentication, updates, hardening)
is the user's responsibility.


## Runtime updates and monitoring

Automatic Tailscale updates are enabled by default. Runtime archives are
authenticated using official signatures chained through signing keys to embedded
upstream roots. Root trust changes require an image update. The updater remains
unprivileged, without a Docker socket or package-manager elevation. Persisted
executable hashes detect accidental modification; they cannot protect a state
volume controlled by an attacker with write access.

Image signatures and labels identify build provenance. The runtime version can
advance beyond the seed. Use observed metrics for the active version, or set
`TAILSCALE_AUTO_UPDATE_ENABLED=false` at creation for an explicit update policy.
Opting out retains the committed runtime version without a downgrade.

The unauthenticated `/metrics` listener belongs on a trusted container network,
not the public Internet. No status/log API exists. State and optional log mounts
must remain private and writable by the configured UID. See
[automatic updates](docs/automatic-updates.md) and the
[authenticated rollout validation plan](docs/testing.md) for recovery limits.
