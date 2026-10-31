package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const defaultShutdownTimeout = 10 * time.Second

const (
	// JobRestartNever preserves the default exit-on-return behavior.
	JobRestartNever = "never"
	// JobRestartOnFailure retries any unexpected job return, including nil.
	JobRestartOnFailure = "on-failure"

	jobRestartInitialBackoff = time.Second
	jobRestartMaxBackoff     = 30 * time.Second
	jobRestartResetAfter     = 10 * time.Minute
	jobRestartLimit          = 5
)

// ErrJobRestartLimit means a job exhausted its consecutive restart allowance.
// The returned error adds only the registered, bounded job name.
var ErrJobRestartLimit = errors.New("worker job restart limit exceeded")

// Config controls the worker's private listeners and optional job supervision.
type Config struct {
	HealthAddr       string
	MetricsAddr      string
	Ready            func(context.Context) error
	SyncSchemaReady  func(context.Context) error
	ShutdownTimeout  time.Duration
	JobRestartPolicy string
}

type healthState struct {
	started   atomic.Bool
	ready     func(context.Context) error
	syncReady func(context.Context) error
}

type runtimeEvent struct {
	kind string
	name string
	err  error
}

// Run starts every registered job in its own supervised goroutine, serves
// health and metrics, and blocks until cancellation or an unexpected exit.
func (r *Registry) Run(ctx context.Context, cfg Config) error {
	return r.run(ctx, cfg, jobRestartTiming{now: time.Now, wait: waitJobRestart})
}

type jobRestartTiming struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) bool
}

func (r *Registry) run(ctx context.Context, cfg Config, timing jobRestartTiming) error {
	if ctx == nil {
		return errors.New("worker context is nil")
	}
	if cfg.HealthAddr == "" {
		return errors.New("worker health address is required")
	}
	if cfg.MetricsAddr == "" {
		return errors.New("worker metrics address is required")
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.ShutdownTimeout < 0 {
		return errors.New("worker shutdown timeout cannot be negative")
	}
	if cfg.JobRestartPolicy != "" && cfg.JobRestartPolicy != JobRestartNever && cfg.JobRestartPolicy != JobRestartOnFailure {
		return errors.New("worker job restart policy must be never or on-failure")
	}

	jobs, err := r.startJobs()
	if err != nil {
		return err
	}
	defer r.stopJobs()
	var restarts *jobRestartMetrics
	metricsHandler := r.metrics.handler()
	if cfg.JobRestartPolicy == JobRestartOnFailure {
		restarts = newJobRestartMetrics(jobs)
		metricsHandler = restarts.handler(metricsHandler)
	}

	healthListener, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("worker health listener %s: %w", cfg.HealthAddr, err)
	}
	metricsListener, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		_ = healthListener.Close()
		return fmt.Errorf("worker metrics listener %s: %w", cfg.MetricsAddr, err)
	}

	state := &healthState{ready: cfg.Ready, syncReady: cfg.SyncSchemaReady}
	servers := []struct {
		name string
		srv  *http.Server
		ln   net.Listener
	}{
		{
			name: "health",
			srv:  &http.Server{Handler: healthHandler(state), ReadHeaderTimeout: 5 * time.Second},
			ln:   healthListener,
		},
		{
			name: "metrics",
			srv:  &http.Server{Handler: metricsHandler, ReadHeaderTimeout: 5 * time.Second},
			ln:   metricsListener,
		},
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan runtimeEvent, len(jobs)+len(servers)+1)
	var jobWG sync.WaitGroup
	for _, job := range jobs {
		r.metrics.setJobRunning(job.Name, true)
		jobWG.Add(1)
		go func() {
			defer jobWG.Done()
			err := r.runJob(runCtx, job, cfg.JobRestartPolicy, timing, restarts)
			events <- runtimeEvent{kind: "job", name: job.Name, err: err}
		}()
	}
	for _, running := range servers {
		go func() {
			if err := running.srv.Serve(running.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				events <- runtimeEvent{kind: "listener", name: running.name, err: err}
			}
		}()
	}
	state.started.Store(true)

	var runErr error
	select {
	case <-ctx.Done():
	case event := <-events:
		if ctx.Err() == nil {
			switch event.kind {
			case "job":
				if cfg.JobRestartPolicy == JobRestartOnFailure && errors.Is(event.err, ErrJobRestartLimit) {
					runErr = event.err
				} else if event.err != nil {
					runErr = fmt.Errorf("worker job %s stopped: %w", event.name, event.err)
				} else {
					runErr = fmt.Errorf("worker job %s stopped unexpectedly", event.name)
				}
			case "listener":
				runErr = fmt.Errorf("worker %s listener stopped: %w", event.name, event.err)
			}
		}
	}

	state.started.Store(false)
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	for _, running := range servers {
		if err := running.srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("shut down worker %s listener: %w", running.name, err)
		}
	}

	jobsDone := make(chan struct{})
	go func() {
		jobWG.Wait()
		close(jobsDone)
	}()
	select {
	case <-jobsDone:
	case <-shutdownCtx.Done():
		if runErr == nil {
			runErr = errors.New("worker jobs did not stop before shutdown timeout")
		}
	}
	return runErr
}

func (r *Registry) runJob(ctx context.Context, job Job, policy string, timing jobRestartTiming, restarts *jobRestartMetrics) error {
	consecutiveRestarts := 0
	for {
		var started time.Time
		if policy == JobRestartOnFailure {
			started = timing.now()
		}
		err := job.Run(ctx)
		result := JobResultCompleted
		if ctx.Err() != nil {
			result = JobResultCanceled
		} else if err != nil {
			result = JobResultError
		}
		r.metrics.observeJobExit(job.Name, result)
		if policy != JobRestartOnFailure || ctx.Err() != nil {
			return err
		}
		if timing.now().Sub(started) >= jobRestartResetAfter {
			consecutiveRestarts = 0
		}
		if consecutiveRestarts == jobRestartLimit {
			return fmt.Errorf("%w: %s", ErrJobRestartLimit, job.Name)
		}
		if !timing.wait(ctx, jobRestartBackoff(consecutiveRestarts)) || ctx.Err() != nil {
			return nil
		}
		consecutiveRestarts++
		restarts.observeRestart(job.Name)
		r.metrics.setJobRunning(job.Name, true)
	}
}

func jobRestartBackoff(restarts int) time.Duration {
	delay := jobRestartInitialBackoff
	for i := 0; i < restarts && delay < jobRestartMaxBackoff; i++ {
		delay = min(delay*2, jobRestartMaxBackoff)
	}
	return delay
}

func waitJobRestart(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Restart metrics exist only for the opt-in policy, so the hosted scrape remains
// byte-identical. Names come exclusively from the frozen job registry.
type jobRestartMetrics struct {
	mu     sync.Mutex
	names  []string
	counts map[string]uint64
}

func newJobRestartMetrics(jobs []Job) *jobRestartMetrics {
	m := &jobRestartMetrics{counts: make(map[string]uint64, len(jobs))}
	for _, job := range jobs {
		m.names = append(m.names, job.Name)
		m.counts[job.Name] = 0
	}
	sort.Strings(m.names)
	return m
}

func (m *jobRestartMetrics) observeRestart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.counts[name]; exists {
		m.counts[name]++
	}
}

func (m *jobRestartMetrics) handler(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		base.ServeHTTP(w, request)
		if request.URL.Path == "/metrics" {
			m.writePrometheus(w)
		}
	})
}

func (m *jobRestartMetrics) writePrometheus(w io.Writer) {
	m.mu.Lock()
	counts := make([]uint64, len(m.names))
	for i, name := range m.names {
		counts[i] = m.counts[name]
	}
	m.mu.Unlock()
	_, _ = fmt.Fprintln(w, "# HELP witself_worker_job_restarts_total Restarts of a registered job after an unexpected return.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_job_restarts_total counter")
	for i, name := range m.names {
		_, _ = fmt.Fprintf(w, "witself_worker_job_restarts_total{job=%q} %d\n", name, counts[i])
	}
}

func healthHandler(state *healthState) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("/livez", ok)
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, _ *http.Request) {
		if !state.started.Load() {
			http.Error(w, "not started", http.StatusServiceUnavailable)
			return
		}
		ok(w, nil)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, request *http.Request) {
		if !state.started.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if state.ready != nil && state.ready(request.Context()) != nil {
			// Never put dependency details, DSNs, or identifiers in a health body.
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		ok(w, nil)
	})
	if state.syncReady != nil {
		mux.HandleFunc("/sync-readyz", func(w http.ResponseWriter, request *http.Request) {
			if !state.started.Load() || state.syncReady(request.Context()) != nil {
				http.Error(w, "local sync schema is not ready", http.StatusServiceUnavailable)
				return
			}
			ok(w, nil)
		})
	}
	return mux
}
