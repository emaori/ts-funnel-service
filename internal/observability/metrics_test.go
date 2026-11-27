package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emaori/ts-funnel-service/internal/updater"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func TestMetricsContractAndNoJSONAPI(t *testing.T) {
	s := updater.NewState(updater.Slot{ID: "b", Version: "v1.104.1"})
	s.PreviousVersion = "v1.102.0"
	s.Attempts["rolled_back"] = 2
	s.LastAttempt = &updater.Attempt{Result: "rolled_back", HadErrors: true, Started: time.Now().Add(-time.Minute), Finished: time.Now(), Duration: 60}
	s.LastError = &updater.ErrorRecord{At: time.Now(), Phase: "validating", Code: "public_probe", Message: "secret diagnostic must never be a label"}
	snap := updater.Snapshot{State: s, Enabled: true, Interval: 6 * time.Hour, Phase: "idle", Observation: updater.Observation{Running: true, Ready: true, Version: s.Active.Version}}
	calls := 0
	server := httptest.NewServer(Handler(NewCollector(func() updater.Snapshot { calls++; return snap }, Build{Service: "test"})))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/openmetrics-text") || !strings.HasSuffix(body, "# EOF\n") {
		t.Fatal("OpenMetrics negotiation failed")
	}
	p := expfmt.NewTextParser(model.LegacyValidation)
	if _, e := p.TextToMetricFamilies(strings.NewReader(body)); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{`role="current",version="v1.104.1"`, `role="previous",version="v1.102.0"`, `result="rolled_back"} 2`, `ts_funnel_update_last_attempt_had_errors{component="tailscale"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(body, "secret diagnostic") || calls != 1 {
		t.Fatal("unexpected scrape side effect or unbounded label")
	}
	for _, path := range []string{"/status", "/logs", "/debug/pprof/"} {
		r, e := http.Get(server.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatal("unexpected API", path)
		}
	}
}
