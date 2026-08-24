// Command witself-support-runner runs Witself's dark, operator-managed AI
// support first responder. It exposes private health and metrics listeners but
// no product API.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/witwave-ai/witself/internal/supportrunner"
	"github.com/witwave-ai/witself/internal/version"
	"github.com/witwave-ai/witself/internal/worker"
)

const supportRunnerJob = "support_runner"

type runner interface {
	Run(context.Context) error
}

type runnerFactory func(supportrunner.Config, func(string)) (runner, error)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	return runWith(args, os.LookupEnv, productionRunner, os.Stdout, os.Stderr)
}

func runWith(
	args []string,
	lookup func(string) (string, bool),
	newRunner runnerFactory,
	stdout, stderr io.Writer,
) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		_, _ = fmt.Fprintln(stdout, version.String("witself-support-runner"))
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	case "serve":
		return serve(lookup, newRunner, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func productionRunner(cfg supportrunner.Config, failureLog func(string)) (runner, error) {
	return supportrunner.New(cfg, failureLog)
}

func serve(
	lookup func(string) (string, bool),
	newRunner runnerFactory,
	stderr io.Writer,
) int {
	cfg, err := supportrunner.FromEnv(lookup)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: configuration: %v\n", err)
		return 1
	}
	if !cfg.Enabled {
		_, _ = fmt.Fprintln(
			stderr,
			"witself-support-runner: WITSELF_SUPPORT_RUNNER_ENABLED=true is required; refusing to serve",
		)
		return 1
	}
	if newRunner == nil {
		_, _ = fmt.Fprintln(stderr, "witself-support-runner: runner factory is nil")
		return 1
	}

	registry := worker.NewRegistry()
	metrics := registry.Metrics()
	r, err := newRunner(cfg, func(ticketID string) {
		metrics.RecordJobFailure(supportRunnerJob)
		// The package records the detailed failure only as a bounded counter.
		// Process logs deliberately contain no customer, account, prompt,
		// response, or provider-error content.
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: ticket %s failed\n", ticketID)
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: construct runner: %v\n", err)
		return 1
	}

	if err := registry.Register(worker.Job{Name: supportRunnerJob, Run: r.Run}); err != nil {
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: register job: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	healthAddr := envOr(lookup, "WITSELF_HEALTH_ADDR", ":8081")
	metricsAddr := envOr(lookup, "WITSELF_METRICS_ADDR", ":9090")
	_, _ = fmt.Fprintf(stderr, "witself-support-runner: health listening on %s\n", healthAddr)
	_, _ = fmt.Fprintf(stderr, "witself-support-runner: metrics listening on %s\n", metricsAddr)
	if err := registry.Run(ctx, worker.Config{
		HealthAddr:  healthAddr,
		MetricsAddr: metricsAddr,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "witself-support-runner: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stderr, "witself-support-runner: shut down cleanly")
	return 0
}

func envOr(lookup func(string) (string, bool), name, fallback string) string {
	if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "witself-support-runner — dark operator-managed AI support first responder")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  witself-support-runner version  Print version information")
	_, _ = fmt.Fprintln(w, "  witself-support-runner serve    Run the support job, health, and metrics listeners")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Dark gate:")
	_, _ = fmt.Fprintln(w, "  WITSELF_SUPPORT_RUNNER_ENABLED=true  required to serve")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Listeners:")
	_, _ = fmt.Fprintln(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	_, _ = fmt.Fprintln(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
}
