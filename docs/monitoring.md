# Monitoring multiple services

`GET /metrics` listens at `0.0.0.0:9090` by default, independently of Tailscale/Caddy. The maintained Prometheus Go client negotiates OpenMetrics and Prometheus text formats. Scrapes read cached snapshots without CLI execution, release requests or update actions.

No JSON status API, log route or exporter sidecar exists. Prometheus/Grafana and other OpenMetrics consumers are optional external tools. The endpoint has no authentication: keep it on a trusted network. `EXPOSE` does not publish a host port; Funnel forwards only to application port 8080.

## Scraping and service names

Connect Prometheus and funnel containers to a shared private network:

```yaml
scrape_configs:
  - job_name: ts-funnel
    scrape_interval: 30s
    static_configs:
      - targets: [ts-funnel-grafana:9090]
        labels:
          service_name: grafana
          environment: production
          host: docker-01
      - targets: [ts-funnel-paperless-1:9090, ts-funnel-paperless-2:9090]
        labels:
          service_name: paperless
          environment: production
          host: docker-01
      - targets: [ts-funnel-immich:9090]
        labels:
          service_name: immich
          environment: production
          host: docker-01
```

`service_name` is a readable deployment label attached by the scraper, separate from `SERVICE_NAME` (Caddy's upstream). Backend naming does not enter application configuration. Service discovery/relabeling can derive names from deployment metadata in the monitoring stack; the application needs no Docker API access.

Keep `instance` unique per container/replica. Joins use `job,instance,environment,host,service_name`; environment/host may be absent. Avoid unintentionally scraping a container under multiple identities.

## Shared dashboard

Import [ts-funnel-service.json](grafana/ts-funnel-service.json) and select a Prometheus data source and job. The portable JSON has no addresses, credentials or installation-specific data-source UID.

The fleet overview shows one row per container with service/instance, versions, update setting, phase, result, times, error category and public-probe freshness. Select a service from its table link or **Inspect service**, then choose replicas through **Instances**. The single detail suite shows all version roles, outcome/error information, readiness history, update/error counters and freshness. The fleet stays visible while inspecting details.

Replicas sharing a service name remain separate. `up` anchors inventory: unreachable targets remain visible with **Unreachable** health and unknown application fields. Zero timestamps display **Never**. Unnamed targets remain identifiable by instance; attach `service_name` for service selection. A target removed from discovery eventually leaves the current inventory.

Newly scraped/labeled targets appear without JSON edits. Configure discovery or add a target in Prometheus first. For a small fleet, optionally make the service variable multi-value and repeat the detail row over explicitly selected services. Keep instance legends; the default does not repeat the complete suite for every container.

The real JSON is tested with Grafana 13.3.2 and Prometheus 3.16.0 and uses built-in panels. Older Grafana versions are not covered. Regenerate with `python3 tools/generate_dashboard.py`; CI rejects stale JSON.

## Metric contract

Names have prefix `ts_funnel_`. Component metrics have `component="tailscale"`; deployment labels come from the scraper. Timestamps are Unix seconds (zero means never), durations are seconds, flags are 0/1. All are gauges unless marked counter.

| Suffix | Meaning / extra labels |
| --- | --- |
| `auto_update_enabled` | Configured policy. |
| `component_running`, `component_ready` | Latest process/local-readiness observations. |
| `component_version_info` | Value 1; `role` and `version`. Roles current, committed, previous, candidate; missing roles omitted. |
| `update_rollback_available` | Outgoing rollback slot retained. |
| `update_check_interval_seconds` | Normal configured interval, before jitter/backoff. |
| `update_last_check_timestamp_seconds` | Latest completed metadata check, including errors. |
| `update_last_successful_check_timestamp_seconds` | Latest successful metadata check. |
| `update_checks_total` | Persistent counter, `result=success\|error`. |
| `update_available` | Latest check found a newer release; clears on commit. |
| `update_candidate_quarantined` | Latest advertised release quarantined. |
| `update_in_progress` | Installation transaction active. |
| `update_phase` | One-hot phase: idle/checking/downloading/verifying/preflighting/switching/validating/rolling_back/recovering. |
| `update_last_attempt_start_timestamp_seconds` | Latest attempt start. |
| `update_last_attempt_finish_timestamp_seconds` | Latest completion; zero during attempt/no attempt. |
| `update_last_success_timestamp_seconds` | Latest successfully committed version change. |
| `update_last_result` | One-hot terminal result: never/success/failed/rolled_back/rollback_failed/interrupted. Previous terminal result retained during a new attempt. |
| `update_last_attempt_had_errors` | Any errors in latest attempt, even after eventual success. |
| `update_attempts_total` | Persistent counter per terminal `result`, excluding never. |
| `update_last_error_timestamp_seconds` | Latest error time; zero if none. |
| `update_last_error_info` | Value 1, bounded `phase` and `code`; absent if none. |
| `update_errors_total` | Persistent counter per `phase`/`code`, emitted after occurrence. |
| `update_last_attempt_duration_seconds` | Completed attempt duration; absent during attempt/never. |
| `update_last_attempt_interruption_seconds` | Interruption estimate; zero before disruptive phase, absent if restoration unconfirmed. |
| `funnel_reachable` | Latest public HTTPS observation. |
| `funnel_last_probe_timestamp_seconds` | Observation time, needed for freshness. |
| `build_info` | Value 1 with immutable `service_version`/`tailscale_version`/`caddy_version`/`alpine_version` provenance. |

`current` is observed, `committed` is the startup choice, `previous` is the last successfully superseded version, and `candidate` is the advertised newer release. Historical previous metadata does not promise the old slot still exists. Image labels remain build provenance after runtime updates.

Scrape availability (`up`), local readiness and public reachability differ. A planned switch keeps metrics up while daemon readiness drops. Public probes are periodic and cached (default five minutes); inspect their age. `STATUS_CHECK_INTERVAL_SECONDS=0` disables periodic observations. Successful retries retain the last error and had-errors flag; interpret them with the result and error time.

Suggested external alerts: rollback failure, quarantine, a newly failed/rolled-back attempt, stale successful checks and local failure outside an update. An old install-success time alone is not a fault if no newer release exists. Alert on counter increases over a window rather than a permanent error total >0.

Full diagnostics stay in `docker logs`/`updates.log`, never labels or a network status API.
