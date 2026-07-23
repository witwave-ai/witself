package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthHandlerSeparatesLivenessStartupAndReadiness(t *testing.T) {
	dependencyErr := errors.New("database secret details")
	state := &healthState{
		ready: func(context.Context) error { return dependencyErr },
	}
	handler := healthHandler(state)

	if status, _ := requestHealth(t, handler, "/livez"); status != http.StatusOK {
		t.Fatalf("/livez before startup = %d, want 200", status)
	}
	if status, _ := requestHealth(t, handler, "/startupz"); status != http.StatusServiceUnavailable {
		t.Fatalf("/startupz before startup = %d, want 503", status)
	}
	if status, body := requestHealth(t, handler, "/readyz"); status != http.StatusServiceUnavailable ||
		strings.Contains(body, dependencyErr.Error()) {
		t.Fatalf("/readyz before startup = %d %q", status, body)
	}

	state.started.Store(true)
	if status, _ := requestHealth(t, handler, "/startupz"); status != http.StatusOK {
		t.Fatalf("/startupz after startup = %d, want 200", status)
	}
	if status, body := requestHealth(t, handler, "/readyz"); status != http.StatusServiceUnavailable ||
		strings.Contains(body, dependencyErr.Error()) {
		t.Fatalf("/readyz with failed dependency = %d %q", status, body)
	}
	state.ready = func(context.Context) error { return nil }
	if status, _ := requestHealth(t, handler, "/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz with healthy dependency = %d, want 200", status)
	}
}

func TestMetricsAreBoundedAndContainRetentionCounts(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Job{Name: "transcript_retention", Run: blockingJob}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Job{Name: "tenant/id", Run: blockingJob}); err == nil {
		t.Fatal("registered an unsafe, unbounded job label")
	}

	metrics := registry.Metrics()
	metrics.now = func() time.Time { return time.Unix(1234, 0) }
	metrics.setJobRunning("transcript_retention", true)
	metrics.RecordJobFailure("transcript_retention")
	metrics.RecordJobFailure("tenant_secret")
	metrics.ObserveRetentionBatch("enforce", RetentionResultSuccess, RetentionCounts{
		Scanned:                7,
		SkippedLocked:          1,
		Eligible:               5,
		Deleted:                4,
		DeferredEvidence:       1,
		ReleasedCurationInputs: 2,
		ScanCapped:             true,
	})
	metrics.ObserveRetentionBatch("account_private", RetentionResultSuccess, RetentionCounts{Deleted: 99})

	var output strings.Builder
	metrics.writePrometheus(&output)
	text := output.String()
	for _, want := range []string{
		`witself_worker_up 1`,
		`witself_worker_job_running{job="transcript_retention"} 1`,
		`witself_worker_job_failures_total{job="transcript_retention"} 1`,
		`witself_worker_retention_batches_total{mode="enforce",result="success"} 1`,
		`witself_worker_retention_items_total{mode="enforce",kind="deleted"} 4`,
		`witself_worker_retention_items_total{mode="enforce",kind="scan_capped_batches"} 1`,
		`witself_worker_retention_last_success_timestamp_seconds{mode="enforce"} 1234`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"tenant/id", "tenant_secret", "account_private", "99"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("metrics exposed unregistered or unbounded value %q:\n%s", forbidden, text)
		}
	}
}

func TestRegistryRunsJobsSeparatelyAndStopsGracefully(t *testing.T) {
	registry := NewRegistry()
	started := make(chan string, 2)
	for _, name := range []string{"avatar_style_rollout", "transcript_retention"} {
		name := name
		if err := registry.Register(Job{
			Name: name,
			Run: func(ctx context.Context) error {
				started <- name
				<-ctx.Done()
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- registry.Run(ctx, Config{
			HealthAddr:      "127.0.0.1:0",
			MetricsAddr:     "127.0.0.1:0",
			ShutdownTimeout: time.Second,
		})
	}()
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for separate job loops")
		}
	}
	if !seen["avatar_style_rollout"] || !seen["transcript_retention"] {
		t.Fatalf("started jobs = %#v", seen)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful runtime stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not stop after cancellation")
	}
}

func TestRegistryCancelsSiblingWhenJobStopsUnexpectedly(t *testing.T) {
	registry := NewRegistry()
	siblingCanceled := make(chan struct{})
	if err := registry.Register(Job{
		Name: "unexpected",
		Run:  func(context.Context) error { return errors.New("boom") },
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Job{
		Name: "sibling",
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			close(siblingCanceled)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	err := registry.Run(context.Background(), Config{
		HealthAddr:      "127.0.0.1:0",
		MetricsAddr:     "127.0.0.1:0",
		ShutdownTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected exit error = %v", err)
	}
	select {
	case <-siblingCanceled:
	default:
		t.Fatal("sibling job was not canceled")
	}
}

func requestHealth(t *testing.T, handler http.Handler, path string) (int, string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response.Code, response.Body.String()
}

func blockingJob(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func TestValidLabelName(t *testing.T) {
	for _, valid := range []string{"a", "avatar_style_rollout", "worker2"} {
		if !validLabelName(valid) {
			t.Errorf("validLabelName(%q) = false", valid)
		}
	}
	if validLabelName("a" + strings.Repeat("b", 63)) {
		t.Error("validLabelName accepted a 64-byte label")
	}
	for _, invalid := range []string{"", "2worker", "UPPER", "tenant/id", "with-dash"} {
		if validLabelName(invalid) {
			t.Errorf("validLabelName(%q) = true", invalid)
		}
	}
}
