package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAuditAppendMetricsReadWorkerStoreCounterOnEveryScrape(t *testing.T) {
	metrics := NewRegistry().Metrics()
	var failures atomic.Uint64
	metrics.SetAuditAppendFailuresReader(failures.Load)
	handler := metrics.handler()

	scrape := func(want uint64) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("metrics status = %d", recorder.Code)
		}
		output := recorder.Body.String()
		for _, line := range []string{
			"# TYPE witself_audit_append_metrics_up gauge\n",
			"witself_audit_append_metrics_up 1\n",
			"# TYPE witself_audit_append_tx_failures_total counter\n",
			fmt.Sprintf("witself_audit_append_tx_failures_total %d\n", want),
		} {
			if !strings.Contains(output, line) {
				t.Errorf("metrics missing %q:\n%s", line, output)
			}
		}
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "witself_audit_append_") && strings.Contains(line, "{") {
				t.Errorf("audit metric acquired labels: %s", line)
			}
		}
	}

	scrape(0)
	// A later successful job can hide the earlier job's error from the worker
	// callback; scraping must still read the independent store failure count.
	failures.Add(1)
	scrape(1)
	scrape(1)
	failures.Add(1)
	scrape(2)
}

func TestAuditAppendMetricsOmitUnconfiguredReader(t *testing.T) {
	metrics := NewRegistry().Metrics()
	metrics.SetAuditAppendFailuresReader(func() uint64 { return 1 })
	metrics.SetAuditAppendFailuresReader(nil)
	var output strings.Builder
	metrics.writePrometheus(&output)
	if strings.Contains(output.String(), "witself_audit_append_") {
		t.Fatalf("unconfigured audit reader exports a healthy counter:\n%s", output.String())
	}
}
