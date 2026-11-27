// Command fixtures serves synthetic OpenMetrics only inside the dashboard smoke network.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/emaori/ts-funnel-service/internal/observability"
	"github.com/emaori/ts-funnel-service/internal/updater"
)

func main() {
	mux := http.NewServeMux()
	for name, data := range map[string]struct {
		version, previous, result string
		errors                    bool
	}{
		"grafana":         {"v1.104.7", "v1.104.1", "success", false},
		"paperless-1":     {"v1.104.1", "v1.102.0", "rolled_back", true},
		"paperless-2":     {"v1.104.7", "v1.104.1", "success", false},
		"updating":        {"v1.104.7", "v1.104.1", "success", false},
		"disabled":        {"v1.104.1", "", "never", false},
		"rollback-failed": {"v1.104.1", "v1.102.0", "rollback_failed", true},
		"new-service":     {"v1.104.7", "", "never", false},
	} {
		s := updater.NewState(updater.Slot{ID: "a", Version: data.version})
		s.PreviousVersion = data.previous
		s.LastResult = data.result
		s.LastCheck = time.Now().Add(-time.Minute)
		s.LastSuccessfulCheck = s.LastCheck
		s.Checks["success"] = 3
		if data.result != "never" {
			s.LastSuccess = time.Now().Add(-2 * time.Hour)
			s.Attempts[data.result] = 1
			n := 2.5
			s.LastAttempt = &updater.Attempt{ID: "fixture", Result: data.result, Started: time.Now().Add(-time.Hour), Finished: time.Now().Add(-time.Hour + 10*time.Second), Duration: 10, Interruption: &n, HadErrors: data.errors}
		}
		if data.errors {
			s.LastError = &updater.ErrorRecord{At: time.Now().Add(-time.Hour), Phase: "validating", Code: "public_probe"}
			s.Errors["validating:public_probe"] = 1
			s.QuarantinedVersion = "v1.104.7"
			s.AvailableVersion = "v1.104.7"
		}
		snap := updater.Snapshot{State: s, Enabled: true, Interval: 6 * time.Hour, Phase: "idle", Observation: updater.Observation{Running: true, Ready: true, ProbeOK: true, Version: data.version, ProbeAt: time.Now()}}
		if name == "disabled" {
			snap.Enabled = false
		}
		if name == "updating" {
			snap.Busy = true
			snap.Phase = "validating"
			snap.Observation.Ready = false
		}
		if name == "rollback-failed" {
			snap.State.Halted = true
			snap.Observation.Ready = false
			snap.Observation.Running = false
		}

		h := observability.Handler(observability.NewCollector(func() updater.Snapshot { return snap }, observability.Build{Service: "fixture"}))
		mux.Handle("/"+name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := r.Clone(r.Context())
			u := *r.URL
			u.Path = "/metrics"
			req.URL = &u
			h.ServeHTTP(w, req)
		}))
	}
	fmt.Fprintln(os.Stdout, "synthetic observability fixtures listening on :9092")
	if err := http.ListenAndServe(":9092", mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
