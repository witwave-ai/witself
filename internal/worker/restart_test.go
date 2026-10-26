package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryNeverRestartPreservesFirstReturn(t *testing.T) {
	for _, policy := range []string{"", JobRestartNever} {
		for _, jobErr := range []error{nil, errors.New("job failure")} {
			t.Run(policy+"/"+errorLabel(jobErr), func(t *testing.T) {
				registry := NewRegistry()
				calls := 0
				if err := registry.Register(Job{Name: "first", Run: func(context.Context) error {
					calls++
					return jobErr
				}}); err != nil {
					t.Fatal(err)
				}
				err := registry.run(context.Background(), Config{
					HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", JobRestartPolicy: policy,
				}, jobRestartTiming{
					now: func() time.Time { t.Error("never policy read restart clock"); return time.Time{} },
					wait: func(context.Context, time.Duration) bool {
						t.Error("never policy waited to restart")
						return false
					},
				})
				want := "worker job first stopped unexpectedly"
				if jobErr != nil {
					want = "worker job first stopped: job failure"
				}
				if err == nil || err.Error() != want || calls != 1 {
					t.Fatalf("Run = %v, calls = %d; want %q and one call", err, calls, want)
				}
				_, body := requestHealth(t, registry.Metrics().handler(), "/metrics")
				if strings.Contains(body, "witself_worker_job_restarts_total") {
					t.Fatal("default metrics gained a restart counter")
				}
			})
		}
	}
}

func TestRegistryServedRestartMetricsFollowPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		policy       string
		wantRestarts bool
	}{
		{name: "hosted"},
		{name: "never", policy: JobRestartNever},
		{name: "on-failure", policy: JobRestartOnFailure, wantRestarts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			started := make(chan struct{})
			if err := registry.Register(Job{Name: "blocking", Run: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			healthAddr, metricsAddr := unusedWorkerAddresses(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Run after cancellation = %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Error("Run did not stop after cancellation")
				}
			})
			go func() {
				done <- registry.Run(ctx, Config{
					HealthAddr: healthAddr, MetricsAddr: metricsAddr, ShutdownTimeout: time.Second, JobRestartPolicy: tc.policy,
				})
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("blocking job did not start")
			}
			client := &http.Client{Timeout: time.Second}
			code, body := getWorkerEndpoint(t, client, "http://"+metricsAddr+"/metrics")
			if code != http.StatusOK || !strings.Contains(body, `witself_worker_job_running{job="blocking"} 1`) {
				t.Fatalf("metrics scrape did not report the running job: status = %d", code)
			}
			if got := strings.Contains(body, "witself_worker_job_restarts_total"); got != tc.wantRestarts {
				t.Fatalf("served restart counter present = %t, want %t for policy %q", got, tc.wantRestarts, tc.policy)
			}
			if tc.wantRestarts && !strings.Contains(body, `witself_worker_job_restarts_total{job="blocking"} 0`) {
				t.Fatal("served restart counter omitted the blocking job's zero value")
			}
		})
	}
}

func TestJobRestartsUnexpectedErrorAndNilReturn(t *testing.T) {
	for _, jobErr := range []error{nil, errors.New("private failure details")} {
		t.Run(errorLabel(jobErr), func(t *testing.T) {
			registry := NewRegistry()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			job := Job{Name: "retry", Run: func(context.Context) error {
				calls++
				if calls == 2 {
					cancel()
					return nil
				}
				return jobErr
			}}
			if err := registry.Register(job); err != nil {
				t.Fatal(err)
			}
			restarts := newJobRestartMetrics([]Job{job})
			var waits []time.Duration
			err := registry.runJob(ctx, job, JobRestartOnFailure, immediateRestartTiming(&waits), restarts)
			if err != nil || calls != 2 || !reflect.DeepEqual(waits, []time.Duration{time.Second}) {
				t.Fatalf("runJob = %v, calls = %d, waits = %v", err, calls, waits)
			}
			_, body := requestHealth(t, restarts.handler(registry.Metrics().handler()), "/metrics")
			result := "completed"
			if jobErr != nil {
				result = "error"
			}
			for _, want := range []string{
				`witself_worker_job_restarts_total{job="retry"} 1`,
				`witself_worker_job_exits_total{job="retry",result="` + result + `"} 1`,
				`witself_worker_job_exits_total{job="retry",result="canceled"} 1`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing metric %s", want)
				}
			}
		})
	}
}

func TestRegistryConsecutiveRestartLimit(t *testing.T) {
	for _, jobErr := range []error{nil, errors.New("private failure details")} {
		t.Run(errorLabel(jobErr), func(t *testing.T) {
			registry := NewRegistry()
			calls := 0
			if err := registry.Register(Job{Name: "exhausted", Run: func(context.Context) error {
				calls++
				return jobErr
			}}); err != nil {
				t.Fatal(err)
			}
			var waits []time.Duration
			err := registry.run(context.Background(), Config{
				HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", JobRestartPolicy: JobRestartOnFailure,
			}, immediateRestartTiming(&waits))
			if !errors.Is(err, ErrJobRestartLimit) || err.Error() != "worker job restart limit exceeded: exhausted" {
				t.Fatalf("restart limit error = %v", err)
			}
			if calls != 6 || !reflect.DeepEqual(waits, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}) {
				t.Fatalf("calls = %d, waits = %v; want initial call plus five restarts", calls, waits)
			}
		})
	}
}

func TestJobRestartBackoffGrowsAndCaps(t *testing.T) {
	for restarts, seconds := range []int{1, 2, 4, 8, 16, 30, 30, 30} {
		if got := jobRestartBackoff(restarts); got != time.Duration(seconds)*time.Second {
			t.Errorf("backoff(%d) = %s, want %ds", restarts, got, seconds)
		}
	}
	if got := jobRestartBackoff(1000); got != 30*time.Second {
		t.Fatalf("backoff must remain capped: %s", got)
	}
}

func TestJobRestartBackoffAndAllowanceResetAfterLongRun(t *testing.T) {
	for _, duration := range []time.Duration{jobRestartResetAfter - time.Nanosecond, jobRestartResetAfter, jobRestartResetAfter + time.Nanosecond} {
		t.Run(duration.String(), func(t *testing.T) {
			registry := NewRegistry()
			now := time.Unix(0, 0)
			calls := 0
			job := Job{Name: "reset", Run: func(context.Context) error {
				calls++
				if calls == 6 {
					now = now.Add(duration)
				}
				return nil
			}}
			if err := registry.Register(job); err != nil {
				t.Fatal(err)
			}
			var waits []time.Duration
			timing := immediateRestartTiming(&waits)
			timing.now = func() time.Time { return now }
			err := registry.runJob(context.Background(), job, JobRestartOnFailure, timing, newJobRestartMetrics([]Job{job}))
			if !errors.Is(err, ErrJobRestartLimit) {
				t.Fatalf("runJob = %v", err)
			}
			want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
			wantCalls := 6
			if duration >= jobRestartResetAfter {
				want = append(want, want...)
				wantCalls = 11
			}
			if calls != wantCalls || !reflect.DeepEqual(waits, want) {
				t.Fatalf("calls = %d, waits = %v; want %d, %v", calls, waits, wantCalls, want)
			}
		})
	}
}

func TestRegistryBackoffKeepsListenersAndSiblingsAliveAndCancelsPromptly(t *testing.T) {
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.Register(Job{Name: "retry", Run: func(context.Context) error {
		calls.Add(1)
		return errors.New("private failure details")
	}}); err != nil {
		t.Fatal(err)
	}
	siblingDone := make(chan struct{})
	if err := registry.Register(Job{Name: "sibling", Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(siblingDone)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	timing := jobRestartTiming{now: time.Now, wait: func(ctx context.Context, delay time.Duration) bool {
		if delay == time.Second {
			return true
		}
		close(waiting)
		return waitJobRestart(ctx, jobRestartMaxBackoff)
	}}
	healthAddr, metricsAddr := unusedWorkerAddresses(t)
	done := make(chan error, 1)
	go func() {
		done <- registry.run(ctx, Config{
			HealthAddr: healthAddr, MetricsAddr: metricsAddr, ShutdownTimeout: time.Second, JobRestartPolicy: JobRestartOnFailure,
		}, timing)
	}()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not enter its second backoff")
	}
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/livez", "/readyz"} {
		code, _ := getWorkerEndpoint(t, client, "http://"+healthAddr+path)
		if code != http.StatusOK {
			t.Errorf("%s during backoff = %d", path, code)
		}
	}
	code, metrics := getWorkerEndpoint(t, client, "http://"+metricsAddr+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics during backoff = %d", code)
	}
	for _, want := range []string{
		`witself_worker_job_restarts_total{job="retry"} 1`,
		`witself_worker_job_restarts_total{job="sibling"} 0`,
		`witself_worker_job_running{job="retry"} 0`,
		`witself_worker_job_exits_total{job="retry",result="error"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing metric %s", want)
		}
	}
	select {
	case <-siblingDone:
		t.Fatal("sibling stopped during retry backoff")
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation during backoff: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt the 30-second backoff")
	}
	if calls.Load() != 2 {
		t.Fatalf("canceled backoff restarted the job: %d calls", calls.Load())
	}
}

func TestJobRestartMetricsIgnoreUnknownNamesAndOtherPaths(t *testing.T) {
	restarts := newJobRestartMetrics([]Job{{Name: "known"}})
	restarts.observeRestart("private_name")
	registry := NewRegistry()
	handler := restarts.handler(registry.Metrics().handler())
	_, metrics := requestHealth(t, handler, "/metrics")
	if strings.Contains(metrics, "private_name") || !strings.Contains(metrics, `witself_worker_job_restarts_total{job="known"} 0`) {
		t.Fatal("restart metrics accepted an unregistered label or omitted registered zero")
	}
	_, missing := requestHealth(t, handler, "/missing")
	if strings.Contains(missing, "witself_worker_job_restarts_total") {
		t.Fatal("restart metrics were appended to a non-metrics response")
	}
}

func TestRegistryRejectsUnknownRestartPolicy(t *testing.T) {
	registry := NewRegistry()
	err := registry.Run(context.Background(), Config{
		HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", JobRestartPolicy: "private_invalid_value",
	})
	if err == nil || err.Error() != "worker job restart policy must be never or on-failure" {
		t.Fatalf("invalid policy = %v", err)
	}
}

func immediateRestartTiming(waits *[]time.Duration) jobRestartTiming {
	return jobRestartTiming{
		now: func() time.Time { return time.Unix(0, 0) },
		wait: func(_ context.Context, delay time.Duration) bool {
			*waits = append(*waits, delay)
			return true
		},
	}
}

func errorLabel(err error) string {
	if err == nil {
		return "nil"
	}
	return "error"
}

func unusedWorkerAddresses(t *testing.T) (string, string) {
	t.Helper()
	health, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = health.Close() }()
	metrics, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Close() }()
	return health.Addr().String(), metrics.Addr().String()
}

func getWorkerEndpoint(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestJobRestartWaitHonorsAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitJobRestart(ctx, time.Hour) {
		t.Fatal("canceled restart wait completed")
	}
}
