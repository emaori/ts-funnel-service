#!/usr/bin/env python3
"""Generate the portable dashboard. Run --check in CI to reject stale JSON."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "docs/grafana/ts-funnel-service.json"
DS = {"type": "prometheus", "uid": "${datasource}"}
KEY = "job,instance,environment,host,service_name"
SCOPE = 'job=~"$job",environment=~"$environment",host=~"$host"'
DETAIL = SCOPE + ',service_name=~"$service",instance=~"$instance"'
COMPONENT = ',component="$component"'
panels = []
panel_id = 0

def metric(name, detail=False):
    return "ts_funnel_" + name + "{" + (DETAIL if detail else SCOPE) + COMPONENT + "}"

def up(detail=False):
    return "up{" + (DETAIL if detail else SCOPE) + "}"

def live(expr, detail=False):
    return "(" + expr + ") and on (" + KEY + ") (" + up(detail) + " == 1)"

def target_key(expr):
    return 'label_join((' + expr + '),"target"," | ","job","instance","environment","host","service_name")'

def target_scalar(expr):
    return "max by (target) (" + target_key(live(expr)) + ")"

def query(expr, ref="A", table=False, instant=True, legend=""):
    return {"refId": ref, "expr": expr, "format": "table" if table else "time_series",
            "instant": instant, "range": not instant, "legendFormat": legend,
            "editorMode": "code", "datasource": DS}

def options(mappings=None, unit="short", no_value="N/A"):
    default = {"unit": unit, "noValue": no_value,
               "color": {"mode": "thresholds"},
               "thresholds": {"mode": "absolute", "steps": [{"color": "green", "value": None}]}}
    if mappings:
        default["mappings"] = [{"type": "value", "options": {
            str(k): {"text": v[0], "color": v[1], "index": i}
            for i, (k, v) in enumerate(mappings.items())}}]
    return {"defaults": default, "overrides": []}

def panel(title, kind, targets, x, y, w, h, field_config=None, extra=None):
    global panel_id
    panel_id += 1
    p = {"id": panel_id, "title": title, "type": kind, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h}, "targets": targets,
         "fieldConfig": field_config or options(), "options": {}}
    if kind == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                        "colorMode": "value", "graphMode": "none", "textMode": "auto",
                        "wideLayout": True, "orientation": "auto"}
    if extra:
        p.update(extra)
    panels.append(p)
    return p

def stat(title, expr, x, y, mappings=None, unit="short", w=4, name_mode=False, no_value="N/A"):
    p = panel(title, "stat", [query(expr, legend="{{instance}}")], x, y, w, 4,
              options(mappings, unit, no_value))
    if name_mode:
        p["options"]["textMode"] = "name"
    return p

def version_info(role, new_label):
    raw = "ts_funnel_component_version_info{" + SCOPE + COMPONENT + ',role="' + role + '"}'
    labeled = 'label_replace((' + live(raw) + '),"' + new_label + '","$1","version","(.*)")'
    return "max by (" + KEY + "," + new_label + ") (" + labeled + ")"

def info_join(base, right, label, empty):
    fallback = 'label_replace((' + up() + '*0+1),"' + label + '","' + empty + '","instance",".*")'
    return "(" + base + ") * on (" + KEY + ") group_left(" + label + ") ((" + right + ") or on (" + KEY + ") (" + fallback + "))"

# The overview does not use $service/$instance. A detail selection never hides
# the fleet. Unreachable targets remain present via the up-based inventory.
busy = metric("update_in_progress")
ready = metric("component_ready")
health = "((" + live("2*(1-" + ready + ")*(1-" + busy + ")+" + busy) + \
         ") or on (" + KEY + ") (3+0*(" + up() + "==0))) or on (" + KEY + ") (2*" + up() + ")"
health = "max by (" + KEY + ") (" + health + ")"
overview = info_join(health, version_info("current", "current_version"), "current_version", "Unavailable")
overview = info_join(overview, version_info("previous", "previous_version"), "previous_version", "None")
for name, source_label, new_label, empty in [
    ("update_last_result", "result", "last_result", "Unknown"),
    ("update_phase", "phase", "update_phase", "Unknown")]:
    rhs = "max by (" + KEY + "," + new_label + ") (" + \
          'label_replace((' + live(metric(name) + "==1") + '),"' + new_label + '","$1","' + source_label + '","(.*)"))'
    overview = info_join(overview, rhs, new_label, empty)
rhs = "max by (" + KEY + ",last_error) (" + \
      'label_join((' + live(metric("update_last_error_info")) + '),"last_error"," / ","code","phase"))'
overview = info_join(overview, rhs, "last_error", "None")
overview = target_key(overview)

stat("Containers", "count(" + up() + ") or vector(0)", 0, 0)
stat("Metrics unreachable", "sum(1-" + up() + ") or vector(0)", 4, 0)
stat("Not locally ready", "count((" + ready + "==0) and on (" + KEY + ") (" + up() + "==1)) or vector(0)", 8, 0)
stat("Updating", "sum(" + live(busy) + ") or vector(0)", 12, 0)
bad = 'ts_funnel_update_last_result{' + SCOPE + COMPONENT + ',result=~"failed|rolled_back|rollback_failed|interrupted"}==1'
stat("Last update needs attention", "sum(" + live(bad) + ") or vector(0)", 16, 0, w=8)

overview_targets = [query(overview, "A", True)]
for ref, expr in [
    ("B", metric("auto_update_enabled")),
    ("C", metric("update_last_success_timestamp_seconds") + "*1000"),
    ("D", metric("update_last_attempt_finish_timestamp_seconds") + "*1000"),
    ("E", metric("update_last_attempt_had_errors")),
    ("F", metric("funnel_reachable")),
    ("G", "time()-(" + metric("funnel_last_probe_timestamp_seconds") + ">0)")]:
    overview_targets.append(query(target_scalar(expr), ref, True))

health_mapping = {0: ("Healthy", "green"), 1: ("Updating", "blue"),
                  2: ("Degraded", "orange"), 3: ("Unreachable", "red")}
table = panel("All service containers — select a service for details", "table", overview_targets,
              0, 4, 24, 11, options(no_value="Unknown"))
table["description"] = "One row per target. Scrape-unreachable targets remain visible. Health is local daemon readiness; Funnel result and probe age are shown separately. Full error messages are in docker logs and updates.log."
table["transformations"] = [
    {"id": "joinByField", "options": {"byField": "target", "mode": "outerTabular"}},
    {"id": "organize", "options": {
        "excludeByName": {"target": True, "job": True, "host": True},
        "indexByName": {"service_name": 0, "instance": 1, "environment": 2,
                       "Value #A": 3, "current_version": 4, "previous_version": 5,
                       "Value #B": 6, "update_phase": 7, "last_result": 8,
                       "Value #C": 9, "Value #D": 10, "Value #E": 11,
                       "last_error": 12, "Value #F": 13, "Value #G": 14},
        "renameByName": {"service_name": "Service", "instance": "Instance", "environment": "Environment",
                         "Value #A": "Health", "current_version": "Current", "previous_version": "Previous",
                         "Value #B": "Auto-update", "update_phase": "Phase", "last_result": "Last result",
                         "Value #C": "Last successful update", "Value #D": "Last attempt",
                         "Value #E": "Errors in attempt", "last_error": "Last error",
                         "Value #F": "Funnel probe", "Value #G": "Probe age"}}}]
table["options"] = {"showHeader": True, "cellHeight": "sm", "sortBy": [{"displayName": "Health", "desc": True}],
                    "footer": {"show": False}}
overrides = table["fieldConfig"]["overrides"]
overrides.append({"matcher": {"id": "byRegexp", "options": "^Time.*$"},
                  "properties": [{"id": "custom.hidden", "value": True}]})
overrides.append({"matcher": {"id": "byName", "options": "Health"},
                  "properties": [{"id": "mappings", "value": options(health_mapping)["defaults"]["mappings"]},
                                 {"id": "custom.cellOptions", "value": {"type": "color-text"}}]})
for name in ["Last successful update", "Last attempt"]:
    overrides.append({"matcher": {"id": "byName", "options": name},
                      "properties": [{"id": "unit", "value": "dateTimeAsIso"},
                                     {"id": "mappings", "value": options({0: ("Never", "text")})["defaults"]["mappings"]}]})
for name in ["Auto-update", "Errors in attempt", "Funnel probe"]:
    overrides.append({"matcher": {"id": "byName", "options": name},
                      "properties": [{"id": "mappings", "value": options({0: ("No", "text"), 1: ("Yes", "text")})["defaults"]["mappings"]}]})
overrides.append({"matcher": {"id": "byName", "options": "Probe age"},
                  "properties": [{"id": "unit", "value": "s"}]})
link = ('/d/ts-funnel-services/tailscale-funnel-services?${__url_time_range}&${datasource:queryparam}&${job:queryparam}'
        '&${environment:queryparam}&${host:queryparam}&${component:queryparam}'
        '&var-service=${__data.fields["Service"]:percentencode}'
        '&var-instance=${__data.fields["Instance"]:percentencode}')
overrides.append({"matcher": {"id": "byName", "options": "Service"},
                  "properties": [{"id": "links", "value": [{"title": "Inspect this service", "url": link, "targetBlank": False}]},
                                 {"id": "noValue", "value": "Name not configured — see Instance"}]})

panel("Selected service: $service", "row", [], 0, 15, 24, 1,
      extra={"collapsed": False, "panels": []})

for i, role in enumerate(["current", "committed", "previous", "candidate"]):
    expr = 'ts_funnel_component_version_info{' + DETAIL + COMPONENT + ',role="' + role + '"}'
    p = stat(role.title() + " version", live(expr, True), i * 6, 16, w=6, name_mode=True)
    p["targets"][0]["legendFormat"] = "{{instance}} · {{version}}"

result_expr = live(metric("update_last_result", True) + "==1", True)
p = stat("Last attempt result", result_expr, 0, 20, w=6, name_mode=True, no_value="Never")
p["targets"][0]["legendFormat"] = "{{instance}} · {{result}}"
phase_expr = live(metric("update_phase", True) + "==1", True)
p = stat("Current phase", phase_expr, 6, 20, w=6, name_mode=True)
p["targets"][0]["legendFormat"] = "{{instance}} · {{phase}}"
stat("Automatic updates", live(metric("auto_update_enabled", True), True), 12, 20,
     {0: ("Disabled", "text"), 1: ("Enabled", "green")}, w=6)
stat("Errors during last attempt", live(metric("update_last_attempt_had_errors", True), True), 18, 20,
     {0: ("No errors", "green"), 1: ("Errors recorded", "orange")}, w=6)
for x, name, title in [
    (0, "update_last_success_timestamp_seconds", "Last successful update"),
    (6, "update_last_attempt_start_timestamp_seconds", "Last attempt started"),
    (12, "update_last_attempt_finish_timestamp_seconds", "Last attempt finished"),
    (18, "update_last_successful_check_timestamp_seconds", "Last successful check")]:
    stat(title, live(metric(name, True) + "*1000", True), x, 24,
         {0: ("Never", "text")}, "dateTimeAsIso", w=6, no_value="Never")
for x, name, title, unit in [
    (0, "update_last_attempt_duration_seconds", "Latest attempt duration", "s"),
    (6, "update_last_attempt_interruption_seconds", "Latest interruption estimate", "s"),
    (12, "update_candidate_quarantined", "Candidate quarantined", "short"),
    (18, "update_rollback_available", "Rollback copy available", "short")]:
    mappings = {0: ("No", "text"), 1: ("Yes", "orange")} if unit == "short" else None
    stat(title, live(metric(name, True), True), x, 28, mappings, unit, w=6)

error_info = live(metric("update_last_error_info", True), True)
p = stat("Last recorded error category", error_info, 0, 32, w=12, name_mode=True, no_value="None")
p["targets"][0]["legendFormat"] = "{{instance}} · {{code}} / {{phase}}"
stat("Last error time", live(metric("update_last_error_timestamp_seconds", True) + "*1000", True),
     12, 32, {0: ("Never", "text")}, "dateTimeAsIso", w=12, no_value="Never")

history = [
    query(up(True), "A", False, False, "{{instance}} · metrics"),
    query(metric("component_ready", True), "B", False, False, "{{instance}} · local ready"),
    query(metric("funnel_reachable", True), "C", False, False, "{{instance}} · public probe")]
p = panel("Readiness and public-path observations", "timeseries", history, 0, 36, 24, 7,
          options(unit="bool"))
p["options"] = {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}
p["fieldConfig"]["defaults"]["custom"] = {"drawStyle": "line", "lineInterpolation": "stepAfter", "fillOpacity": 10}

attempts = 'increase(ts_funnel_update_attempts_total{' + DETAIL + COMPONENT + '}[$__rate_interval])'
p = panel("Update outcomes over time", "timeseries",
          [query(attempts, instant=False, legend="{{instance}} · {{result}}")],
          0, 43, 12, 7)
p["options"] = {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}
errors = 'increase(ts_funnel_update_errors_total{' + DETAIL + COMPONENT + '}[$__rate_interval])'
p = panel("Update errors over time", "timeseries",
          [query(errors, instant=False, legend="{{instance}} · {{phase}} / {{code}}")],
          12, 43, 12, 7)
p["options"] = {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}
stat("Metadata check age", "time()-(" + live(metric("update_last_successful_check_timestamp_seconds", True) + ">0", True) + ")",
     0, 50, unit="s", w=8)
stat("Public probe age", "time()-(" + live(metric("funnel_last_probe_timestamp_seconds", True) + ">0", True) + ")",
     8, 50, unit="s", w=8)
stat("Normal check interval", live(metric("update_check_interval_seconds", True), True), 16, 50, unit="s", w=8)

def variable(name, label, expression, *, multi=False, all_value=False, current=None):
    v = {"name": name, "label": label, "type": "query", "datasource": DS,
         "definition": expression, "query": {"query": expression, "refId": name},
         "refresh": 1, "sort": 1, "multi": multi, "includeAll": all_value, "options": []}
    if all_value:
        v["allValue"] = ".*"
        v["current"] = {"text": "All", "value": "$__all"}
    else:
        v["current"] = current or {}
    return v

variables = [
    {"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus",
     "refresh": 1, "current": {}, "options": []},
    variable("job", "Job", "label_values(up, job)", multi=True,
             current={"text": "ts-funnel", "value": ["ts-funnel"]}),
    variable("environment", "Environment", 'label_values(up{job=~"$job"}, environment)', multi=True, all_value=True),
    variable("host", "Host", 'label_values(up{job=~"$job",environment=~"$environment"}, host)', multi=True, all_value=True),
    variable("service", "Inspect service", "label_values(" + up() + ", service_name)"),
    variable("instance", "Instances", 'label_values(up{' + SCOPE + ',service_name=~"$service"}, instance)', multi=True, all_value=True),
    {"name": "component", "label": "Component", "type": "custom", "query": "tailscale",
     "current": {"text": "tailscale", "value": "tailscale"},
     "options": [{"text": "tailscale", "value": "tailscale", "selected": True}]}]

dashboard = {"uid": "ts-funnel-services", "title": "Tailscale Funnel Services", "schemaVersion": 39,
             "version": 1, "editable": True, "timezone": "browser", "tags": ["tailscale", "ts-funnel", "openmetrics"],
             "description": "One shared fleet dashboard. Select a service in the overview for details. Full diagnostics remain in docker logs and updates.log.",
             "time": {"from": "now-24h", "to": "now"}, "refresh": "30s",
             "templating": {"list": variables}, "panels": panels, "annotations": {"list": []}}

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    content = json.dumps(dashboard, indent=2, ensure_ascii=False) + "\n"
    if args.check:
        if not OUTPUT.exists() or OUTPUT.read_text() != content:
            raise SystemExit("Dashboard JSON is stale; run python3 tools/generate_dashboard.py")
    else:
        OUTPUT.parent.mkdir(parents=True, exist_ok=True)
        OUTPUT.write_text(content)

if __name__ == "__main__":
    main()
