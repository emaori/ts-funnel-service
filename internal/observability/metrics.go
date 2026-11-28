package observability

import (
	"net/http"
	"strings"
	"time"

	"github.com/emaori/ts-funnel-service/internal/updater"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var Phases = []string{"idle", "checking", "downloading", "verifying", "preflighting", "switching", "validating", "rolling_back", "recovering"}
var Results = []string{"never", "success", "failed", "rolled_back", "rollback_failed", "interrupted"}

type Build struct{ Service, Tailscale, Caddy, Alpine string }
type Collector struct {
	snapshot func() updater.Snapshot
	build    Build
	desc     map[string]*prometheus.Desc
}

func NewCollector(snapshot func() updater.Snapshot, build Build) *Collector {
	c := &Collector{snapshot: snapshot, build: build, desc: map[string]*prometheus.Desc{}}
	scalars := map[string]string{
		"auto_update_enabled":                            "Whether automatic updates are enabled.",
		"component_running":                              "Production daemon process liveness.",
		"component_ready":                                "Validated local production readiness.",
		"update_rollback_available":                      "Whether an outgoing rollback slot is retained.",
		"update_check_interval_seconds":                  "Configured normal update check interval.",
		"update_last_check_timestamp_seconds":            "Last completed metadata check.",
		"update_last_successful_check_timestamp_seconds": "Last successful metadata check.",
		"update_available":                               "A newer eligible release was found by the latest successful check.",
		"update_candidate_quarantined":                   "The latest candidate is quarantined.",
		"update_in_progress":                             "An installation transaction is in progress.",
		"update_last_attempt_start_timestamp_seconds":    "Start of the latest installation attempt.",
		"update_last_attempt_finish_timestamp_seconds":   "Completion of the latest installation attempt.",
		"update_last_success_timestamp_seconds":          "Last successfully committed version change.",
		"update_last_attempt_had_errors":                 "Whether the latest attempt encountered any errors.",
		"update_last_error_timestamp_seconds":            "Most recent error occurrence.",
		"update_last_attempt_duration_seconds":           "Duration of the latest completed attempt.",
		"update_last_attempt_interruption_seconds":       "Estimated interruption of the latest restored attempt.",
		"funnel_reachable":                               "Latest public Funnel probe result, interpreted with its timestamp.",
		"funnel_last_probe_timestamp_seconds":            "Latest public Funnel probe observation time.",
	}
	for name, help := range scalars {
		c.desc[name] = prometheus.NewDesc("ts_funnel_"+name, help, nil, prometheus.Labels{"component": "tailscale"})
	}
	for name, labels := range map[string][]string{
		"component_version_info": {"role", "version"}, "update_checks_total": {"result"}, "update_phase": {"phase"},
		"update_last_result": {"result"}, "update_attempts_total": {"result"}, "update_errors_total": {"phase", "code"}, "update_last_error_info": {"phase", "code"},
	} {
		c.desc[name] = prometheus.NewDesc("ts_funnel_"+name, strings.ReplaceAll(name, "_", " ")+".", labels, prometheus.Labels{"component": "tailscale"})
	}
	c.desc["build_info"] = prometheus.NewDesc("ts_funnel_build_info", "Immutable image build provenance.", []string{"service_version", "tailscale_version", "caddy_version", "alpine_version"}, nil)
	return c
}
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.desc {
		ch <- d
	}
}
func unix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snap := c.snapshot()
	s := snap.State
	o := snap.Observation
	gauge := func(name string, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(c.desc[name], prometheus.GaugeValue, v, labels...)
	}
	counter := func(name string, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(c.desc[name], prometheus.CounterValue, float64(v), labels...)
	}
	for name, value := range map[string]float64{
		"auto_update_enabled": flag(snap.Enabled), "component_running": flag(o.Running), "component_ready": flag(o.Ready),
		"update_rollback_available": flag(s.RollbackSlot != ""), "update_check_interval_seconds": snap.Interval.Seconds(),
		"update_last_check_timestamp_seconds": unix(s.LastCheck), "update_last_successful_check_timestamp_seconds": unix(s.LastSuccessfulCheck),
		"update_available": flag(s.AvailableVersion != ""), "update_candidate_quarantined": flag(s.AvailableVersion != "" && s.AvailableVersion == s.QuarantinedVersion),
		"update_in_progress": flag(snap.Busy), "update_last_success_timestamp_seconds": unix(s.LastSuccess),
		"funnel_reachable": flag(o.ProbeOK), "funnel_last_probe_timestamp_seconds": unix(o.ProbeAt),
	} {
		gauge(name, value)
	}
	for role, version := range map[string]string{"committed": s.Active.Version, "previous": s.PreviousVersion, "candidate": s.AvailableVersion} {
		if version != "" {
			gauge("component_version_info", 1, role, version)
		}
	}
	if o.Running && o.Version != "" {
		gauge("component_version_info", 1, "current", o.Version)
	}
	for _, phase := range Phases {
		gauge("update_phase", flag(phase == snap.Phase), phase)
	}
	last := s.LastResult
	if last == "" {
		last = "never"
	}
	var start, finish time.Time
	var hadErrors bool
	if a := s.LastAttempt; a != nil {
		start = a.Started
		finish = a.Finished
		hadErrors = a.HadErrors
		if a.Result != "" {
			last = a.Result
		}
		if !a.Finished.IsZero() {
			gauge("update_last_attempt_duration_seconds", a.Duration)
			if a.Interruption != nil {
				gauge("update_last_attempt_interruption_seconds", *a.Interruption)
			}
		}
	}
	gauge("update_last_attempt_start_timestamp_seconds", unix(start))
	gauge("update_last_attempt_finish_timestamp_seconds", unix(finish))
	gauge("update_last_attempt_had_errors", flag(hadErrors))
	for _, result := range Results {
		gauge("update_last_result", flag(result == last), result)
		if result != "never" {
			counter("update_attempts_total", s.Attempts[result], result)
		}
	}
	for _, result := range []string{"success", "error"} {
		counter("update_checks_total", s.Checks[result], result)
	}
	for key, value := range s.Errors {
		phase, code, ok := strings.Cut(key, ":")
		if ok {
			counter("update_errors_total", value, phase, code)
		}
	}
	var errorAt time.Time
	if s.LastError != nil {
		errorAt = s.LastError.At
		gauge("update_last_error_info", 1, s.LastError.Phase, s.LastError.Code)
	}
	gauge("update_last_error_timestamp_seconds", unix(errorAt))
	gauge("build_info", 1, c.build.Service, c.build.Tailscale, c.build.Caddy, c.build.Alpine)
}
func Handler(c *Collector) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	return mux
}
