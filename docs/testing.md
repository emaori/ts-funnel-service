# Validation and rollout test plan

## Automated checks

Use the Go version from `go.mod` and Docker Buildx. Ordinary tests need no auth key.

```bash
go test -race ./...
go vet ./...
python3 tools/generate_dashboard.py --check
docker buildx build --load -f tests/Dockerfile.native -t ts-funnel-native-test .
docker run --rm ts-funnel-native-test
docker buildx build --load -f tests/Dockerfile.observability -t ts-funnel-fixtures .
docker buildx build --load -f tests/Dockerfile.browser -t ts-funnel-browser .
python3 tests/observability_smoke.py \
  --fixture-image ts-funnel-fixtures --browser-image ts-funnel-browser
docker buildx build --load -t ts-funnel-test .
```

The real official-package download test is opt-in:

```bash
TS_FUNNEL_ONLINE_TEST=1 go test -run TestOfficialDistribution -v ./internal/tailscale
```

Fault tests cover three successive A/B updates; identity/state retention; verification, preflight and baseline failures; stop timeout; state-copy/commit failures; rollback and failed rollback; quarantine/retry; interrupted phases and committed-before-cleanup recovery; exactly-once counters; corrupt metadata; binary tampering; legacy migration; and exclusive volume use. Injected drivers are not proof of upstream compatibility.

The native suite runs real Tailscale/Caddy as UID 1000. It checks isolated unauthenticated preflight, LocalAPI version/startup, explicit native updater disable and generated/custom read-only Caddyfile validation. CI runs native tests on amd64/arm64 using QEMU for arm64 on its disposable runner.

The monitoring suite creates isolated uniquely named containers and no host-published ports. It imports the real dashboard into clean Grafana, runs every query against Prometheus, preserves unreachable targets/replicas, and adds a new target without JSON edits. The browser check verifies rendered columns and service links retaining filters/time range. No test services enter the application image.

## Evidence and limits

Development validation used Linux amd64, UID 1000, Tailscale v1.104.1, Caddy 2.11.4 and Alpine 3.24.2. The online test verified the official signed v1.104.7 package and matching executables. Monitoring tests pin Grafana 13.3.2 and Prometheus 3.16.0.

An authenticated real Funnel upgrade/rollback and measured public interruption have not been validated in this workspace. No dedicated test credential was supplied; production credentials/services were not used. Local arm64 cross-compilation is distinct from native execution, which is delegated to CI. Fake-driver results do not establish a real upgrade-gap/rollback compatibility matrix.

## Authenticated release gate

Before releasing unattended updates, use disposable dedicated nodes, separate volumes and a deterministic upstream on supported architectures/runtimes:

1. Start an older supported stable version with a single-use test auth key and Funnel permission. Record node ID/DNS, certificate, Serve config, state inventory and Caddy PID.
2. Confirm public Funnel resolution from the container, including a public-view DNS override if needed. Test stable 200, authentication and redirect baselines. Poll public HTTPS externally throughout handover.
3. Perform two successive real updates proving A→B and B→A. Verify node ID/URL/certificate continuity, versions, exit-before-start ordering, unchanged Caddy PID and continuous metrics availability. Measure external request outage rather than treating timeout budgets as outage measurements.
4. Recreate against the same volume without an auth key, with updates disabled and with an older baked-in seed. Verify the committed slot is retained and no auth key is reused by handover/preflight.
5. Inject startup/version/identity/Funnel/public-path failures and candidate state migration failure. Verify restoration of untouched outgoing state or visible `rollback_failed` with halted updates. Test expired credentials and independent control-plane/upstream outages.
6. Kill at each durable boundary: staging, preflight, outgoing stop, copy, candidate start, validation, commit and cleanup. Restart and verify pair selection, no new enrollment and exactly-once counters. A durably accepted candidate must survive interrupted cleanup.
7. Exercise stop-timeout/deadline exit, full/read-only storage and unavailable log mounts/rotation. No concurrent production identities or blocked restoration are allowed.
8. Verify Docker output, default persistent logs, separate bind and named log volumes, UID permissions and separate Docker/file retention.
9. Exercise never-updated, disabled, busy, success-with-errors, failed, rollback, failed recovery and stale/unreachable dashboard states with several services and replicas.
10. Record old/new release pairs, architecture/runtime/filesystem, public DNS setup, interruption distribution and recovery results. Use this evidence to establish supported version gaps and rollback compatibility.

State copying preserves regular daemon-managed files and the outgoing snapshot. Only authenticated live tests establish real identity/certificate/Tailnet Lock behavior across specific upstream releases and external control-plane constraints.
