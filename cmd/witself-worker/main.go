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
	messageRetentionJob    = "message_retention"
	agentEmailRetentionJob = "agent_email_retention"

	avatarStyleRolloutEnabledEnv      = "WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED"
	avatarStyleRolloutBatchSizeEnv    = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE"
	avatarStyleRolloutIntervalEnv     = "WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL"
	avatarStyleRolloutBatchTimeoutEnv = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT"

	transcriptRetentionEnabledEnv      = "WITSELF_TRANSCRIPT_RETENTION_ENABLED"
	transcriptRetentionModeEnv         = "WITSELF_TRANSCRIPT_RETENTION_MODE"
	transcriptRetentionBatchSizeEnv    = "WITSELF_TRANSCRIPT_RETENTION_BATCH_SIZE"
	transcriptRetentionIntervalEnv     = "WITSELF_TRANSCRIPT_RETENTION_INTERVAL"
	transcriptRetentionBatchTimeoutEnv = "WITSELF_TRANSCRIPT_RETENTION_BATCH_TIMEOUT"

	messageRetentionEnabledEnv      = "WITSELF_MESSAGE_RETENTION_ENABLED"
	messageRetentionModeEnv         = "WITSELF_MESSAGE_RETENTION_MODE"
	messageRetentionBatchSizeEnv    = "WITSELF_MESSAGE_RETENTION_BATCH_SIZE"
	messageRetentionIntervalEnv     = "WITSELF_MESSAGE_RETENTION_INTERVAL"
	messageRetentionBatchTimeoutEnv = "WITSELF_MESSAGE_RETENTION_BATCH_TIMEOUT"

	agentEmailRetentionEnabledEnv      = "WITSELF_AGENT_EMAIL_RETENTION_ENABLED"
	agentEmailRetentionModeEnv         = "WITSELF_AGENT_EMAIL_RETENTION_MODE"
	agentEmailRetentionBatchSizeEnv    = "WITSELF_AGENT_EMAIL_RETENTION_BATCH_SIZE"
	agentEmailRetentionIntervalEnv     = "WITSELF_AGENT_EMAIL_RETENTION_INTERVAL"
	agentEmailRetentionBatchTimeoutEnv = "WITSELF_AGENT_EMAIL_RETENTION_BATCH_TIMEOUT"
)

type jobConfig struct {
	avatarEnabled              bool
	avatar                     store.AvatarStyleRolloutWorkerConfig
	retentionEnabled           bool
	retention                  store.TranscriptRetentionWorkerConfig
	messageRetentionEnabled    bool
	messageRetention           store.MessageRetentionWorkerConfig
	agentEmailRetentionEnabled bool
	agentEmailRetention        store.AgentEmailRetentionWorkerConfig
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
	if jobs.messageRetentionEnabled {
		cfg := jobs.messageRetention
		mode := string(cfg.Mode)
		if err := registry.Register(worker.Job{
			Name: messageRetentionJob,
			Run: func(jobCtx context.Context) error {
				return st.RunMessageRetentionWorker(
					jobCtx,
					cfg,
					func(result store.MessageRetentionBatchResult) {
						metrics.ObserveMessageRetentionBatch(
							mode,
							messageRetentionMetricResult(result),
							messageRetentionMetricCounts(result),
						)
						logMessageRetentionResult(cfg.Mode, result)
					},
					func(err error) {
						metrics.RecordJobFailure(messageRetentionJob)
						metrics.ObserveMessageRetentionBatch(
							mode,
							worker.RetentionResultError,
							worker.MessageRetentionCounts{},
						)
						fmt.Fprintf(os.Stderr, "witself-worker: message retention: %v\n", err)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "witself-worker: register message retention: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr,
			"witself-worker: message retention enabled (mode %s, batch %d, interval %s, timeout %s)\n",
			cfg.Mode, cfg.BatchSize, cfg.Interval, cfg.BatchTimeout)
	}
	if jobs.agentEmailRetentionEnabled {
		cfg := jobs.agentEmailRetention
		mode := string(cfg.Mode)
		if err := registry.Register(worker.Job{
			Name: agentEmailRetentionJob,
			Run: func(jobCtx context.Context) error {
				return st.RunAgentEmailRetentionWorker(
					jobCtx,
					cfg,
					func(result store.AgentEmailRetentionBatchResult) {
						metrics.ObserveAgentEmailRetentionBatch(
							mode,
							agentEmailRetentionMetricResult(result),
							agentEmailRetentionMetricCounts(result),
						)
						logAgentEmailRetentionResult(cfg.Mode, result)
					},
					func(err error) {
						metrics.RecordJobFailure(agentEmailRetentionJob)
						metrics.ObserveAgentEmailRetentionBatch(
							mode,
							worker.RetentionResultError,
							worker.AgentEmailRetentionCounts{},
						)
						fmt.Fprintf(
							os.Stderr,
							"witself-worker: agent-email retention: %v\n",
							err,
						)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"witself-worker: register agent-email retention: %v\n",
				err,
			)
			return 1
		}
		fmt.Fprintf(
			os.Stderr,
			"witself-worker: agent-email retention enabled (mode %s, batch %d, interval %s, timeout %s)\n",
			cfg.Mode, cfg.BatchSize, cfg.Interval, cfg.BatchTimeout,
		)
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

	messageRetentionEnabled, err := boolEnv(lookup, messageRetentionEnabledEnv, false)
	if err != nil {
		return jobConfig{}, err
	}
	messageRetention := store.DefaultMessageRetentionWorkerConfig()
	if raw, ok := lookup(messageRetentionModeEnv); ok {
		messageRetention.Mode = store.MessageRetentionMode(
			strings.ToLower(strings.TrimSpace(raw)),
		)
	}
	if raw, ok := lookup(messageRetentionBatchSizeEnv); ok {
		messageRetention.BatchSize, err = parseIntEnv(messageRetentionBatchSizeEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(messageRetentionIntervalEnv); ok {
		messageRetention.Interval, err = parseDurationEnv(messageRetentionIntervalEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(messageRetentionBatchTimeoutEnv); ok {
		messageRetention.BatchTimeout, err = parseDurationEnv(
			messageRetentionBatchTimeoutEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := messageRetention.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s/%s message retention configuration: %w",
			messageRetentionModeEnv,
			messageRetentionBatchSizeEnv,
			messageRetentionIntervalEnv,
			messageRetentionBatchTimeoutEnv,
			err,
		)
	}
	agentEmailRetentionEnabled, err := boolEnv(
		lookup,
		agentEmailRetentionEnabledEnv,
		false,
	)
	if err != nil {
		return jobConfig{}, err
	}
	agentEmailRetention := store.DefaultAgentEmailRetentionWorkerConfig()
	if raw, ok := lookup(agentEmailRetentionModeEnv); ok {
		agentEmailRetention.Mode = store.AgentEmailRetentionMode(
			strings.ToLower(strings.TrimSpace(raw)),
		)
	}
	if raw, ok := lookup(agentEmailRetentionBatchSizeEnv); ok {
		agentEmailRetention.BatchSize, err = parseIntEnv(
			agentEmailRetentionBatchSizeEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailRetentionIntervalEnv); ok {
		agentEmailRetention.Interval, err = parseDurationEnv(
			agentEmailRetentionIntervalEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailRetentionBatchTimeoutEnv); ok {
		agentEmailRetention.BatchTimeout, err = parseDurationEnv(
			agentEmailRetentionBatchTimeoutEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := agentEmailRetention.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s/%s agent-email retention configuration: %w",
			agentEmailRetentionModeEnv,
			agentEmailRetentionBatchSizeEnv,
			agentEmailRetentionIntervalEnv,
			agentEmailRetentionBatchTimeoutEnv,
			err,
		)
	}
	return jobConfig{
		avatarEnabled:              avatarEnabled,
		avatar:                     avatar,
		retentionEnabled:           retentionEnabled,
		retention:                  retention,
		messageRetentionEnabled:    messageRetentionEnabled,
		messageRetention:           messageRetention,
		agentEmailRetentionEnabled: agentEmailRetentionEnabled,
		agentEmailRetention:        agentEmailRetention,
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

func messageRetentionMetricResult(
	result store.MessageRetentionBatchResult,
) worker.RetentionResult {
	if result.Scanned == 0 &&
		result.EligibleThreads == 0 &&
		result.DeletedThreads == 0 &&
		result.DeferredEvidence == 0 &&
		result.DeferredActive == 0 &&
		result.DeferredLocked == 0 &&
		result.DeferredOversize == 0 &&
		result.DeferredBudget == 0 &&
		result.RepairedActivity == 0 {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func messageRetentionMetricCounts(
	result store.MessageRetentionBatchResult,
) worker.MessageRetentionCounts {
	return worker.MessageRetentionCounts{
		Scanned:          result.Scanned,
		SkippedLocked:    result.SkippedLocked,
		EligibleThreads:  result.EligibleThreads,
		DeletedThreads:   result.DeletedThreads,
		DeletedMessages:  result.DeletedMessages,
		DeferredEvidence: result.DeferredEvidence,
		DeferredActive:   result.DeferredActive,
		DeferredLocked:   result.DeferredLocked,
		DeferredOversize: result.DeferredOversize,
		DeferredBudget:   result.DeferredBudget,
		RepairedActivity: result.RepairedActivity,
		ScanCapped:       result.ScanCapped,
	}
}

func agentEmailRetentionMetricResult(
	result store.AgentEmailRetentionBatchResult,
) worker.RetentionResult {
	if result.Scanned == 0 &&
		result.Eligible == 0 &&
		result.Deleted == 0 &&
		result.DeletedRawBytes == 0 &&
		result.DeferredActive == 0 &&
		result.DeferredLocked == 0 &&
		result.DeferredOversize == 0 &&
		result.DeferredBudget == 0 &&
		result.ClearedDuplicateLinks == 0 &&
		result.DeletedCanaryProofs == 0 {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func agentEmailRetentionMetricCounts(
	result store.AgentEmailRetentionBatchResult,
) worker.AgentEmailRetentionCounts {
	return worker.AgentEmailRetentionCounts{
		Scanned:               result.Scanned,
		SkippedLocked:         result.SkippedLocked,
		Eligible:              result.Eligible,
		Deleted:               result.Deleted,
		DeletedRawBytes:       result.DeletedRawBytes,
		DeferredActive:        result.DeferredActive,
		DeferredLocked:        result.DeferredLocked,
		DeferredOversize:      result.DeferredOversize,
		DeferredBudget:        result.DeferredBudget,
		ClearedDuplicateLinks: result.ClearedDuplicateLinks,
		DeletedCanaryProofs:   result.DeletedCanaryProofs,
		ScanCapped:            result.ScanCapped,
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

func logMessageRetentionResult(
	mode store.MessageRetentionMode,
	result store.MessageRetentionBatchResult,
) {
	if messageRetentionMetricResult(result) == worker.RetentionResultNoWork {
		return
	}
	fmt.Fprintf(os.Stderr,
		"witself-worker: message retention: mode=%s scanned=%d skipped_locked=%d scan_capped=%t eligible_threads=%d deleted_threads=%d deleted_messages=%d deferred_evidence=%d deferred_active=%d deferred_locked=%d deferred_oversize=%d deferred_budget=%d repaired_activity=%d\n",
		mode,
		result.Scanned,
		result.SkippedLocked,
		result.ScanCapped,
		result.EligibleThreads,
		result.DeletedThreads,
		result.DeletedMessages,
		result.DeferredEvidence,
		result.DeferredActive,
		result.DeferredLocked,
		result.DeferredOversize,
		result.DeferredBudget,
		result.RepairedActivity,
	)
}

func logAgentEmailRetentionResult(
	mode store.AgentEmailRetentionMode,
	result store.AgentEmailRetentionBatchResult,
) {
	if agentEmailRetentionMetricResult(result) == worker.RetentionResultNoWork {
		return
	}
	fmt.Fprintf(
		os.Stderr,
		"witself-worker: agent-email retention: mode=%s scanned=%d skipped_locked=%d scan_capped=%t eligible=%d deleted=%d deleted_raw_bytes=%d deferred_active=%d deferred_locked=%d deferred_oversize=%d deferred_budget=%d cleared_duplicate_links=%d deleted_canary_proofs=%d\n",
		mode,
		result.Scanned,
		result.SkippedLocked,
		result.ScanCapped,
		result.Eligible,
		result.Deleted,
		result.DeletedRawBytes,
		result.DeferredActive,
		result.DeferredLocked,
		result.DeferredOversize,
		result.DeferredBudget,
		result.ClearedDuplicateLinks,
		result.DeletedCanaryProofs,
	)
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
