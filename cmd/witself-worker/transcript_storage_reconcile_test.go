package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

func TestTranscriptStorageReconcileConfigFromEnv(t *testing.T) {
	jobs, err := jobConfigFromEnv(mapLookup(nil))
	if err != nil || jobs.transcriptStorageReconcileEnabled || jobs.transcriptStorageReconcile != store.DefaultTranscriptStorageReconcileConfig() {
		t.Fatalf("unexpected defaults: %+v %v", jobs.transcriptStorageReconcile, err)
	}
	if err := registerTranscriptStorageReconcile(nil, nil, jobs, nil); err != nil {
		t.Fatalf("disabled registration: %v", err)
	}
	values := map[string]string{
		transcriptStorageReconcileEnv + "ENABLED":      "true",
		transcriptStorageReconcileEnv + "INTERVAL":     "30s",
		transcriptStorageReconcileEnv + "DUTY_CYCLE":   "0.25",
		transcriptStorageReconcileEnv + "PAGE_ENTRIES": "25",
		transcriptStorageReconcileEnv + "UNIT_TIMEOUT": "60s",
	}
	jobs, err = jobConfigFromEnv(mapLookup(values))
	if err != nil || !jobs.transcriptStorageReconcileEnabled || jobs.transcriptStorageReconcile != (store.TranscriptStorageReconcileConfig{Interval: 30 * time.Second, DutyCycle: .25, PageEntries: 25, UnitTimeout: time.Minute}) {
		t.Fatalf("configured: %+v %v", jobs.transcriptStorageReconcile, err)
	}
	r := worker.NewRegistry()
	var output bytes.Buffer
	if err := registerTranscriptStorageReconcile(r, nil, jobs, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "witself-worker: transcript storage reconcile enabled (interval 30s, duty 0.25, page 25, unit timeout 1m0s)\n" {
		t.Fatalf("startup line: %s", output.String())
	}
	if err := registerTranscriptStorageReconcile(r, nil, jobs, &output); err == nil {
		t.Fatal("enabled job was not registered")
	}
	for _, invalid := range []struct{ key, value string }{
		{"ENABLED", "maybe"}, {"INTERVAL", "bad"}, {"INTERVAL", "9s"}, {"INTERVAL", "61m"},
		{"DUTY_CYCLE", "abc"}, {"DUTY_CYCLE", "0.04"}, {"DUTY_CYCLE", "1.01"}, {"DUTY_CYCLE", "NaN"}, {"DUTY_CYCLE", "Inf"},
		{"PAGE_ENTRIES", "abc"}, {"PAGE_ENTRIES", "0"}, {"PAGE_ENTRIES", "10001"},
		{"UNIT_TIMEOUT", "bad"}, {"UNIT_TIMEOUT", "9s"}, {"UNIT_TIMEOUT", "281s"},
	} {
		t.Run(invalid.key+"/"+invalid.value, func(t *testing.T) {
			if _, err := jobConfigFromEnv(mapLookup(map[string]string{transcriptStorageReconcileEnv + invalid.key: invalid.value})); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestTranscriptStorageReconcileReportValueFree(t *testing.T) {
	r := worker.NewRegistry()
	var output bytes.Buffer
	jobs := jobConfig{transcriptStorageReconcileEnabled: true, transcriptStorageReconcile: store.DefaultTranscriptStorageReconcileConfig()}
	if err := registerTranscriptStorageReconcile(r, nil, jobs, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	reportTranscriptStorageReconcile(r.Metrics(), &output, store.TranscriptStorageReconcilePassResult{LeaseHeld: true})
	if output.Len() != 0 {
		t.Fatal("lease-held pass logged work")
	}
	reportTranscriptStorageReconcile(r.Metrics(), &output, store.TranscriptStorageReconcilePassResult{Timeout: 2, Error: 3, Anomaly: 1, Entries: 10, Err: errors.New("private-account-id")})
	if strings.Contains(output.String(), "private-account-id") || strings.Count(output.String(), "\n") != 1 || !strings.Contains(output.String(), "timeout=2 error=3") {
		t.Fatalf("unexpected report: %s", output.String())
	}
}

func TestTranscriptStorageReconcileReportJobFailures(t *testing.T) {
	r := worker.NewRegistry()
	started := make(chan struct{})
	if err := r.Register(worker.Job{Name: transcriptStorageReconcileJob, Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}

	// The public worker runtime exposes the same rendering used by Prometheus.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsAddr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- r.Run(ctx, worker.Config{
			HealthAddr: "127.0.0.1:0", MetricsAddr: metricsAddr, ShutdownTimeout: time.Second,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runtimeDone:
			if err != nil {
				t.Errorf("worker runtime: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker runtime did not stop")
		}
	})
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker runtime did not start")
	}

	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	failures := func(t *testing.T) uint64 {
		t.Helper()
		response, err := client.Get("http://" + metricsAddr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("metrics status: %d", response.StatusCode)
		}
		const prefix = `witself_worker_job_failures_total{job="transcript_storage_reconcile"} `
		for line := range strings.SplitSeq(string(body), "\n") {
			if value, ok := strings.CutPrefix(line, prefix); ok {
				count, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					t.Fatalf("job failure sample: %v", err)
				}
				return count
			}
		}
		t.Fatal("missing transcript reconcile job failure sample")
		return 0
	}
	for _, test := range []struct {
		name   string
		result store.TranscriptStorageReconcilePassResult
		want   uint64
	}{
		{"pass_error", store.TranscriptStorageReconcilePassResult{Err: errors.New("pass failed")}, 1},
		{"timeout", store.TranscriptStorageReconcilePassResult{Timeout: 1}, 1},
		{"error", store.TranscriptStorageReconcilePassResult{Error: 1}, 1},
		{"anomaly", store.TranscriptStorageReconcilePassResult{Anomaly: 1}, 1},
		{"busy", store.TranscriptStorageReconcilePassResult{Busy: 1}, 0},
		{"raced", store.TranscriptStorageReconcilePassResult{Raced: 1}, 0},
		{"skipped", store.TranscriptStorageReconcilePassResult{Skipped: 1}, 0},
		{"corrected", store.TranscriptStorageReconcilePassResult{Corrected: 1}, 0},
		{"confirmed", store.TranscriptStorageReconcilePassResult{Confirmed: 1}, 0},
		{"interval_cut_off", store.TranscriptStorageReconcilePassResult{Confirmed: 1, Elapsed: 11 * time.Second}, 0},
		{"lease_held", store.TranscriptStorageReconcilePassResult{LeaseHeld: true}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := failures(t)
			reportTranscriptStorageReconcile(r.Metrics(), io.Discard, test.result)
			if got := failures(t) - before; got != test.want {
				t.Fatalf("job failure increment = %d, want %d", got, test.want)
			}
		})
	}
}
