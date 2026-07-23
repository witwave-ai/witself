// Command witself-worker runs Witself's durable cell-local background jobs.
// It exposes health and Prometheus listeners, but never the product API.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
	"github.com/witwave-ai/witself/internal/worker"
)

const (
	avatarStyleRolloutJob  = "avatar_style_rollout"
	transcriptRetentionJob = "transcript_retention"

	avatarStyleRolloutEnabledEnv      = "WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED"
	avatarStyleRolloutBatchSizeEnv    = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE"
	avatarStyleRolloutIntervalEnv     = "WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL"
	avatarStyleRolloutBatchTimeoutEnv = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT"

	transcriptRetentionEnabledEnv      = "WITSELF_TRANSCRIPT_RETENTION_ENABLED"
	transcriptRetentionModeEnv         = "WITSELF_TRANSCRIPT_RETENTION_MODE"
	transcriptRetentionBatchSizeEnv    = "WITSELF_TRANSCRIPT_RETENTION_BATCH_SIZE"
	transcriptRetentionIntervalEnv     = "WITSELF_TRANSCRIPT_RETENTION_INTERVAL"
	transcriptRetentionBatchTimeoutEnv = "WITSELF_TRANSCRIPT_RETENTION_BATCH_TIMEOUT"
)

type jobConfig struct {
	avatarEnabled    bool
	avatar           store.AvatarStyleRolloutWorkerConfig
	retentionEnabled bool
	retention        store.TranscriptRetentionWorkerConfig
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println(version.String("witself-worker"))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "serve":
		return serve()
	default:
		fmt.Fprintf(os.Stderr, "witself-worker: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func serve() int {
	jobs, err := jobConfigFromEnv(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-worker: %v\n", err)
		return 1
	}
	dsn := dbDSN(os.LookupEnv)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "witself-worker: WITSELF_DATABASE_URL is required (falls back to DATABASE_URL)")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-worker: database: %v\n", err)
		return 1
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		fmt.Fprintf(os.Stderr, "witself-worker: migrate: %v\n", err)
		return 1
	}
	if err := st.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "witself-worker: database readiness: %v\n", err)
		return 1
	}

	registry := worker.NewRegistry()
	metrics := registry.Metrics()
	if jobs.avatarEnabled {
		cfg := jobs.avatar
		if err := registry.Register(worker.Job{
			Name: avatarStyleRolloutJob,
			Run: func(jobCtx context.Context) error {
				return st.RunAvatarStyleRolloutWorker(jobCtx, cfg, func(err error) {
					metrics.RecordJobFailure(avatarStyleRolloutJob)
					fmt.Fprintf(os.Stderr, "witself-worker: avatar style rollout: %v\n", err)
				})
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "witself-worker: register avatar style rollout: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr,
			"witself-worker: avatar style rollout enabled (batch %d, interval %s, timeout %s)\n",
			cfg.BatchSize, cfg.Interval, cfg.BatchTimeout)
	}
	if jobs.retentionEnabled {
		cfg := jobs.retention
		mode := string(cfg.Mode)
		if err := registry.Register(worker.Job{
			Name: transcriptRetentionJob,
			Run: func(jobCtx context.Context) error {
				return st.RunTranscriptRetentionWorker(
					jobCtx,
					cfg,
					func(result store.TranscriptRetentionBatchResult) {
						metrics.ObserveRetentionBatch(mode, retentionMetricResult(result), retentionMetricCounts(result))
						logRetentionResult(cfg.Mode, result)
					},
					func(err error) {
						metrics.RecordJobFailure(transcriptRetentionJob)
						metrics.ObserveRetentionBatch(mode, worker.RetentionResultError, worker.RetentionCounts{})
						fmt.Fprintf(os.Stderr, "witself-worker: transcript retention: %v\n", err)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "witself-worker: register transcript retention: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr,
			"witself-worker: transcript retention enabled (mode %s, batch %d, interval %s, timeout %s)\n",
			cfg.Mode, cfg.BatchSize, cfg.Interval, cfg.BatchTimeout)
	}

	healthAddr := envOr(os.LookupEnv, "WITSELF_HEALTH_ADDR", ":8081")
	metricsAddr := envOr(os.LookupEnv, "WITSELF_METRICS_ADDR", ":9090")
	fmt.Fprintln(os.Stderr, "witself-worker: migrated; database ready")
	fmt.Fprintf(os.Stderr, "witself-worker: health listening on %s\n", healthAddr)
	fmt.Fprintf(os.Stderr, "witself-worker: metrics listening on %s\n", metricsAddr)
	if err := registry.Run(ctx, worker.Config{
		HealthAddr:  healthAddr,
		MetricsAddr: metricsAddr,
		Ready:       st.Ping,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "witself-worker: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "witself-worker: shut down cleanly")
	return 0
}

func jobConfigFromEnv(lookup func(string) (string, bool)) (jobConfig, error) {
	avatarEnabled, err := boolEnv(lookup, avatarStyleRolloutEnabledEnv, true)
	if err != nil {
		return jobConfig{}, err
	}
	avatar := store.DefaultAvatarStyleRolloutWorkerConfig()
	if raw, ok := lookup(avatarStyleRolloutBatchSizeEnv); ok {
		avatar.BatchSize, err = parseIntEnv(avatarStyleRolloutBatchSizeEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(avatarStyleRolloutIntervalEnv); ok {
		avatar.Interval, err = parseDurationEnv(avatarStyleRolloutIntervalEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(avatarStyleRolloutBatchTimeoutEnv); ok {
		avatar.BatchTimeout, err = parseDurationEnv(avatarStyleRolloutBatchTimeoutEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := avatar.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s avatar style rollout configuration: %w",
			avatarStyleRolloutBatchSizeEnv,
			avatarStyleRolloutIntervalEnv,
			avatarStyleRolloutBatchTimeoutEnv,
			err,
		)
	}

	retentionEnabled, err := boolEnv(lookup, transcriptRetentionEnabledEnv, false)
	if err != nil {
		return jobConfig{}, err
	}
	retention := store.DefaultTranscriptRetentionWorkerConfig()
	if raw, ok := lookup(transcriptRetentionModeEnv); ok {
		retention.Mode = store.TranscriptRetentionMode(strings.ToLower(strings.TrimSpace(raw)))
	}
	if raw, ok := lookup(transcriptRetentionBatchSizeEnv); ok {
		retention.BatchSize, err = parseIntEnv(transcriptRetentionBatchSizeEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(transcriptRetentionIntervalEnv); ok {
		retention.Interval, err = parseDurationEnv(transcriptRetentionIntervalEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(transcriptRetentionBatchTimeoutEnv); ok {
		retention.BatchTimeout, err = parseDurationEnv(transcriptRetentionBatchTimeoutEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := retention.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s/%s transcript retention configuration: %w",
			transcriptRetentionModeEnv,
			transcriptRetentionBatchSizeEnv,
			transcriptRetentionIntervalEnv,
			transcriptRetentionBatchTimeoutEnv,
			err,
		)
	}
	return jobConfig{
		avatarEnabled:    avatarEnabled,
		avatar:           avatar,
		retentionEnabled: retentionEnabled,
		retention:        retention,
	}, nil
}

func boolEnv(lookup func(string) (string, bool), key string, defaultValue bool) (bool, error) {
	raw, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return value, nil
}

func parseIntEnv(key, raw string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func parseDurationEnv(key, raw string) (time.Duration, error) {
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return value, nil
}

func dbDSN(lookup func(string) (string, bool)) string {
	if value, ok := lookup("WITSELF_DATABASE_URL"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if value, ok := lookup("DATABASE_URL"); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func envOr(lookup func(string) (string, bool), key, fallback string) string {
	if value, ok := lookup(key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func retentionMetricResult(result store.TranscriptRetentionBatchResult) worker.RetentionResult {
	if result == (store.TranscriptRetentionBatchResult{}) {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func retentionMetricCounts(result store.TranscriptRetentionBatchResult) worker.RetentionCounts {
	return worker.RetentionCounts{
		Scanned:                result.Scanned,
		SkippedLocked:          result.SkippedLocked,
		Eligible:               result.Eligible,
		Deleted:                result.Deleted,
		DeferredEvidence:       result.DeferredEvidence,
		DeferredCuration:       result.DeferredCuration,
		ReleasedCurationInputs: result.ReleasedCurationInputs,
		DeletedCurationCursors: result.DeletedCurationCursors,
		ScanCapped:             result.ScanCapped,
		EligibleScanCapped:     result.EligibleScanCapped,
		DeferredScanCapped:     result.DeferredScanCapped,
	}
}

func logRetentionResult(mode store.TranscriptRetentionMode, result store.TranscriptRetentionBatchResult) {
	if result == (store.TranscriptRetentionBatchResult{}) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"witself-worker: transcript retention: mode=%s scanned=%d skipped_locked=%d scan_capped=%t eligible=%d eligible_scan_capped=%t deleted=%d deferred_evidence=%d deferred_curation=%d deferred_scan_capped=%t released_curation_inputs=%d deleted_curation_cursors=%d\n",
		mode, result.Scanned, result.SkippedLocked, result.ScanCapped, result.Eligible,
		result.EligibleScanCapped, result.Deleted, result.DeferredEvidence,
		result.DeferredCuration, result.DeferredScanCapped,
		result.ReleasedCurationInputs, result.DeletedCurationCursors)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "witself-worker — durable Witself cell background jobs")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  witself-worker version    Print version information")
	_, _ = fmt.Fprintln(w, "  witself-worker serve      Run jobs, health, and metrics listeners")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Required:")
	_, _ = fmt.Fprintln(w, "  WITSELF_DATABASE_URL  Postgres DSN (falls back to DATABASE_URL)")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Listeners:")
	_, _ = fmt.Fprintln(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	_, _ = fmt.Fprintln(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
}
