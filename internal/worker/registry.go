// Package worker provides the process runtime shared by durable Witself
// background workers. It supervises independent long-lived jobs and exposes
// dependency-free health and Prometheus endpoints without a product API.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Job is one independently supervised long-lived worker loop.
type Job struct {
	Name string
	Run  func(context.Context) error
}

// Registry owns a bounded set of process-defined jobs and their metrics.
// Registration is complete before Run starts; runtime data never creates labels.
type Registry struct {
	mu      sync.Mutex
	running bool
	jobs    []Job
	names   map[string]struct{}
	metrics *Metrics
}

// NewRegistry returns an empty worker registry.
func NewRegistry() *Registry {
	return &Registry{
		names:   make(map[string]struct{}),
		metrics: newMetrics(),
	}
}

// Metrics returns the registry's process-local metrics recorder.
func (r *Registry) Metrics() *Metrics {
	return r.metrics
}

// Register adds one process-defined job. Names are deliberately restricted so
// they remain a bounded, safe Prometheus label rather than tenant input.
func (r *Registry) Register(job Job) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if !validLabelName(job.Name) {
		return fmt.Errorf("worker job name %q must match [a-z][a-z0-9_]{0,62}", job.Name)
	}
	if job.Run == nil {
		return fmt.Errorf("worker job %q has no run function", job.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return errors.New("worker jobs cannot be registered after runtime start")
	}
	if _, exists := r.names[job.Name]; exists {
		return fmt.Errorf("worker job %q is already registered", job.Name)
	}
	r.jobs = append(r.jobs, job)
	r.names[job.Name] = struct{}{}
	r.metrics.registerJob(job.Name)
	return nil
}

func (r *Registry) startJobs() ([]Job, error) {
	if r == nil {
		return nil, errors.New("worker registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return nil, errors.New("worker registry is already running")
	}
	r.running = true
	return append([]Job(nil), r.jobs...), nil
}

func (r *Registry) stopJobs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
}

func validLabelName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
