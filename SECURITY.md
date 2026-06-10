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
