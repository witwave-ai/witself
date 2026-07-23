package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const defaultShutdownTimeout = 10 * time.Second

// Config controls the worker's private health and Prometheus listeners.
type Config struct {
	HealthAddr      string
	MetricsAddr     string
	Ready           func(context.Context) error
	ShutdownTimeout time.Duration
}

type healthState struct {
	started atomic.Bool
	ready   func(context.Context) error
}

type runtimeEvent struct {
	kind string
	name string
	err  error
}

// Run starts every registered job in its own supervised goroutine, serves
// health and metrics, and blocks until cancellation or an unexpected exit.
func (r *Registry) Run(ctx context.Context, cfg Config) error {
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

	jobs, err := r.startJobs()
	if err != nil {
		return err
	}
	defer r.stopJobs()

	healthListener, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("worker health listener %s: %w", cfg.HealthAddr, err)
	}
	metricsListener, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		_ = healthListener.Close()
		return fmt.Errorf("worker metrics listener %s: %w", cfg.MetricsAddr, err)
	}

	state := &healthState{ready: cfg.Ready}
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
			srv:  &http.Server{Handler: r.metrics.handler(), ReadHeaderTimeout: 5 * time.Second},
			ln:   metricsListener,
		},
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan runtimeEvent, len(jobs)+len(servers)+1)
	var jobWG sync.WaitGroup
	for _, job := range jobs {
		job := job
		r.metrics.setJobRunning(job.Name, true)
		jobWG.Add(1)
		go func() {
			defer jobWG.Done()
			err := job.Run(runCtx)
			result := JobResultCompleted
			if runCtx.Err() != nil {
				result = JobResultCanceled
			} else if err != nil {
				result = JobResultError
			}
			r.metrics.observeJobExit(job.Name, result)
			events <- runtimeEvent{kind: "job", name: job.Name, err: err}
		}()
	}
	for _, running := range servers {
		running := running
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
				if event.err != nil {
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
	return mux
}
