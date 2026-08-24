// Command witself-worker runs Witself's durable cell-local background jobs.
// It exposes health and Prometheus listeners, but never the product API.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
	"github.com/witwave-ai/witself/internal/worker"
)

const (
	avatarStyleRolloutJob          = "avatar_style_rollout"
	messageRateBucketCleanupJob    = "message_rate_bucket_cleanup"
	agentEmailRateBucketCleanupJob = "agent_email_rate_bucket_cleanup"
	agentEmailOutboundJob          = "agent_email_outbound"
	transcriptRetentionJob         = "transcript_retention"
	messageRetentionJob            = "message_retention"
	agentEmailRetentionJob         = "agent_email_retention"
	accountPurgeJob                = "account_purge"

	avatarStyleRolloutEnabledEnv      = "WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED"
	avatarStyleRolloutBatchSizeEnv    = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE"
	avatarStyleRolloutIntervalEnv     = "WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL"
	avatarStyleRolloutBatchTimeoutEnv = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT"

	messageRateBucketCleanupEnabledEnv      = "WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_ENABLED"
	messageRateBucketCleanupBatchSizeEnv    = "WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_SIZE"
	messageRateBucketCleanupIntervalEnv     = "WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_INTERVAL"
	messageRateBucketCleanupBatchTimeoutEnv = "WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT"

	agentEmailRateBucketCleanupEnabledEnv      = "WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_ENABLED"
	agentEmailRateBucketCleanupBatchSizeEnv    = "WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_SIZE"
	agentEmailRateBucketCleanupIntervalEnv     = "WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_INTERVAL"
	agentEmailRateBucketCleanupBatchTimeoutEnv = "WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT"

	agentEmailOutboundEnabledEnv            = "WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED"
	agentEmailOutboundDispatchEndpointEnv   = "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT"
	agentEmailOutboundDispatchAudienceEnv   = "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE"
	agentEmailOutboundDispatchKeyIDEnv      = "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID"
	agentEmailOutboundDispatchPrivateKeyEnv = "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY"
	agentEmailOutboundBatchSizeEnv          = "WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_SIZE"
	agentEmailOutboundIntervalEnv           = "WITSELF_AGENT_EMAIL_OUTBOUND_INTERVAL"
	agentEmailOutboundBatchTimeoutEnv       = "WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_TIMEOUT"
	agentEmailOutboundProviderTimeoutEnv    = "WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT"

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

	accountPurgeEnabledEnv      = "WITSELF_ACCOUNT_PURGE_ENABLED"
	accountPurgeModeEnv         = "WITSELF_ACCOUNT_PURGE_MODE"
	accountPurgeBatchSizeEnv    = "WITSELF_ACCOUNT_PURGE_BATCH_SIZE"
	accountPurgeIntervalEnv     = "WITSELF_ACCOUNT_PURGE_INTERVAL"
	accountPurgeBatchTimeoutEnv = "WITSELF_ACCOUNT_PURGE_BATCH_TIMEOUT"
	accountPurgeGraceEnv        = "WITSELF_ACCOUNT_PURGE_GRACE"
)

type jobConfig struct {
	avatarEnabled                      bool
	avatar                             store.AvatarStyleRolloutWorkerConfig
	messageRateBucketCleanupEnabled    bool
	messageRateBucketCleanup           store.MessageRateBucketCleanupWorkerConfig
	agentEmailRateBucketCleanupEnabled bool
	agentEmailRateBucketCleanup        store.AgentEmailRateBucketCleanupWorkerConfig
	agentEmailOutboundEnabled          bool
	agentEmailOutbound                 agentEmailOutboundWorkerConfig
	agentEmailOutboundClient           agentemailoutbound.Client
	retentionEnabled                   bool
	retention                          store.TranscriptRetentionWorkerConfig
	messageRetentionEnabled            bool
	messageRetention                   store.MessageRetentionWorkerConfig
	agentEmailRetentionEnabled         bool
	agentEmailRetention                store.AgentEmailRetentionWorkerConfig
	accountPurgeEnabled                bool
	accountPurge                       store.AccountPurgeWorkerConfig
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
	case "agent-email":
		if len(args) >= 2 && args[1] == "receipt-replay" {
			return runAgentEmailReceiptReplay(args[2:])
		}
		fmt.Fprintln(os.Stderr, "witself-worker: unknown agent-email command")
		return 2
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
	if jobs.messageRateBucketCleanupEnabled {
		cfg := jobs.messageRateBucketCleanup
		if err := registry.Register(worker.Job{
			Name: messageRateBucketCleanupJob,
			Run: func(jobCtx context.Context) error {
				return st.RunMessageRateBucketCleanupWorker(
					jobCtx,
					cfg,
					func(deleted int64) {
						metrics.ObserveMessageRateBucketCleanupBatch(
							messageRateBucketCleanupMetricResult(deleted),
							deleted,
						)
						if deleted > 0 {
							fmt.Fprintf(
								os.Stderr,
								"witself-worker: message rate bucket cleanup: deleted=%d\n",
								deleted,
							)
						}
					},
					func(err error) {
						metrics.RecordJobFailure(messageRateBucketCleanupJob)
						metrics.ObserveMessageRateBucketCleanupBatch(
							worker.RetentionResultError,
							0,
						)
						fmt.Fprintf(
							os.Stderr,
							"witself-worker: message rate bucket cleanup: %v\n",
							err,
						)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"witself-worker: register message rate bucket cleanup: %v\n",
				err,
			)
			return 1
		}
		fmt.Fprintf(
			os.Stderr,
			"witself-worker: message rate bucket cleanup enabled (batch %d, interval %s, timeout %s)\n",
			cfg.BatchSize,
			cfg.Interval,
			cfg.BatchTimeout,
		)
	}
	if jobs.agentEmailRateBucketCleanupEnabled {
		cfg := jobs.agentEmailRateBucketCleanup
		if err := registry.Register(worker.Job{
			Name: agentEmailRateBucketCleanupJob,
			Run: func(jobCtx context.Context) error {
				return st.RunAgentEmailRateBucketCleanupWorker(
					jobCtx,
					cfg,
					func(deleted int64) {
						metrics.ObserveAgentEmailRateBucketCleanupBatch(
							agentEmailRateBucketCleanupMetricResult(deleted),
							deleted,
						)
						if deleted > 0 {
							fmt.Fprintf(
								os.Stderr,
								"witself-worker: agent-email rate bucket cleanup: deleted=%d\n",
								deleted,
							)
						}
					},
					func(err error) {
						metrics.RecordJobFailure(agentEmailRateBucketCleanupJob)
						metrics.ObserveAgentEmailRateBucketCleanupBatch(
							worker.RetentionResultError,
							0,
						)
						fmt.Fprintf(
							os.Stderr,
							"witself-worker: agent-email rate bucket cleanup: %v\n",
							err,
						)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"witself-worker: register agent-email rate bucket cleanup: %v\n",
				err,
			)
			return 1
		}
		fmt.Fprintf(
			os.Stderr,
			"witself-worker: agent-email rate bucket cleanup enabled (batch %d, interval %s, timeout %s)\n",
			cfg.BatchSize,
			cfg.Interval,
			cfg.BatchTimeout,
		)
	}
	if jobs.agentEmailOutboundEnabled {
		cfg := jobs.agentEmailOutbound
		dispatchClient := jobs.agentEmailOutboundClient
		dispatchClient.HTTPClient = &http.Client{Timeout: cfg.ProviderTimeout}
		if err := registry.Register(worker.Job{
			Name: agentEmailOutboundJob,
			Run: func(jobCtx context.Context) error {
				return runAgentEmailOutboundWorker(
					jobCtx,
					st,
					&dispatchClient,
					cfg,
					func(result agentEmailOutboundBatchResult) {
						metrics.ObserveAgentEmailOutboundBatch(
							agentEmailOutboundMetricResult(result),
							agentEmailOutboundMetricCounts(result),
						)
						logAgentEmailOutboundResult(result)
					},
					func(err error) {
						metrics.RecordJobFailure(agentEmailOutboundJob)
						metrics.ObserveAgentEmailOutboundBatch(
							worker.RetentionResultError,
							worker.AgentEmailOutboundCounts{},
						)
						fmt.Fprintf(os.Stderr, "witself-worker: outbound agent email: %v\n", err)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "witself-worker: register outbound agent email: %v\n", err)
			return 1
		}
		fmt.Fprintf(
			os.Stderr,
			"witself-worker: outbound agent email enabled (batch %d, interval %s, batch timeout %s, provider timeout %s)\n",
			cfg.BatchSize,
			cfg.Interval,
			cfg.BatchTimeout,
			cfg.ProviderTimeout,
		)
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
					func(result store.AgentEmailRetentionBatchResult, err error) {
						metricResult := agentEmailRetentionMetricResult(result)
						if err != nil {
							metrics.RecordJobFailure(agentEmailRetentionJob)
							metricResult = worker.RetentionResultError
						}
						metrics.ObserveAgentEmailRetentionBatch(
							mode,
							metricResult,
							agentEmailRetentionMetricCounts(result),
						)
						if result != (store.AgentEmailRetentionBatchResult{}) {
							logAgentEmailRetentionResult(cfg.Mode, result)
						}
						if err == nil {
							return
						}
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
	if jobs.accountPurgeEnabled {
		cfg := jobs.accountPurge
		mode := string(cfg.Mode)
		if err := registry.Register(worker.Job{
			Name: accountPurgeJob,
			Run: func(jobCtx context.Context) error {
				return st.RunAccountPurgeWorker(
					jobCtx,
					cfg,
					func(result store.AccountPurgeBatchResult) {
						metrics.ObserveAccountPurgeBatch(
							mode,
							accountPurgeMetricResult(result),
							accountPurgeMetricCounts(result),
						)
						logAccountPurgeResult(cfg.Mode, result)
					},
					func(err error) {
						metrics.RecordJobFailure(accountPurgeJob)
						metrics.ObserveAccountPurgeBatch(
							mode,
							worker.RetentionResultError,
							worker.AccountPurgeCounts{},
						)
						fmt.Fprintf(os.Stderr, "witself-worker: account purge: %v\n", err)
					},
				)
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "witself-worker: register account purge: %v\n", err)
			return 1
		}
		fmt.Fprintf(
			os.Stderr,
			"witself-worker: account purge enabled (mode %s, batch %d, interval %s, timeout %s, grace %s)\n",
			cfg.Mode,
			cfg.BatchSize,
			cfg.Interval,
			cfg.BatchTimeout,
			cfg.Grace,
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

	messageRateBucketCleanupEnabled, err := boolEnv(
		lookup,
		messageRateBucketCleanupEnabledEnv,
		true,
	)
	if err != nil {
		return jobConfig{}, err
	}
	messageRateBucketCleanup := store.DefaultMessageRateBucketCleanupWorkerConfig()
	if raw, ok := lookup(messageRateBucketCleanupBatchSizeEnv); ok {
		messageRateBucketCleanup.BatchSize, err = parseIntEnv(
			messageRateBucketCleanupBatchSizeEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(messageRateBucketCleanupIntervalEnv); ok {
		messageRateBucketCleanup.Interval, err = parseDurationEnv(
			messageRateBucketCleanupIntervalEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(messageRateBucketCleanupBatchTimeoutEnv); ok {
		messageRateBucketCleanup.BatchTimeout, err = parseDurationEnv(
			messageRateBucketCleanupBatchTimeoutEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := messageRateBucketCleanup.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s message rate bucket cleanup configuration: %w",
			messageRateBucketCleanupBatchSizeEnv,
			messageRateBucketCleanupIntervalEnv,
			messageRateBucketCleanupBatchTimeoutEnv,
			err,
		)
	}

	agentEmailRateBucketCleanupEnabled, err := boolEnv(
		lookup,
		agentEmailRateBucketCleanupEnabledEnv,
		true,
	)
	if err != nil {
		return jobConfig{}, err
	}
	agentEmailRateBucketCleanup := store.DefaultAgentEmailRateBucketCleanupWorkerConfig()
	if raw, ok := lookup(agentEmailRateBucketCleanupBatchSizeEnv); ok {
		agentEmailRateBucketCleanup.BatchSize, err = parseIntEnv(
			agentEmailRateBucketCleanupBatchSizeEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailRateBucketCleanupIntervalEnv); ok {
		agentEmailRateBucketCleanup.Interval, err = parseDurationEnv(
			agentEmailRateBucketCleanupIntervalEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailRateBucketCleanupBatchTimeoutEnv); ok {
		agentEmailRateBucketCleanup.BatchTimeout, err = parseDurationEnv(
			agentEmailRateBucketCleanupBatchTimeoutEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := agentEmailRateBucketCleanup.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s agent-email rate bucket cleanup configuration: %w",
			agentEmailRateBucketCleanupBatchSizeEnv,
			agentEmailRateBucketCleanupIntervalEnv,
			agentEmailRateBucketCleanupBatchTimeoutEnv,
			err,
		)
	}

	agentEmailOutboundEnabled, err := boolEnv(lookup, agentEmailOutboundEnabledEnv, false)
	if err != nil {
		return jobConfig{}, err
	}
	agentEmailOutbound := defaultAgentEmailOutboundWorkerConfig()
	if raw, ok := lookup(agentEmailOutboundBatchSizeEnv); ok {
		agentEmailOutbound.BatchSize, err = parseIntEnv(agentEmailOutboundBatchSizeEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailOutboundIntervalEnv); ok {
		agentEmailOutbound.Interval, err = parseDurationEnv(agentEmailOutboundIntervalEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailOutboundBatchTimeoutEnv); ok {
		agentEmailOutbound.BatchTimeout, err = parseDurationEnv(agentEmailOutboundBatchTimeoutEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(agentEmailOutboundProviderTimeoutEnv); ok {
		agentEmailOutbound.ProviderTimeout, err = parseDurationEnv(
			agentEmailOutboundProviderTimeoutEnv,
			raw,
		)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := agentEmailOutbound.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s/%s outbound agent-email configuration: %w",
			agentEmailOutboundBatchSizeEnv,
			agentEmailOutboundIntervalEnv,
			agentEmailOutboundBatchTimeoutEnv,
			agentEmailOutboundProviderTimeoutEnv,
			err,
		)
	}
	agentEmailOutboundClient := agentemailoutbound.Client{
		Audience: "witself-agent-email-send",
	}
	if raw, ok := lookup(agentEmailOutboundDispatchEndpointEnv); ok {
		agentEmailOutboundClient.Endpoint = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(agentEmailOutboundDispatchAudienceEnv); ok {
		agentEmailOutboundClient.Audience = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(agentEmailOutboundDispatchKeyIDEnv); ok {
		agentEmailOutboundClient.KeyID = strings.TrimSpace(raw)
	}
	if agentEmailOutboundEnabled {
		rawKey, ok := lookup(agentEmailOutboundDispatchPrivateKeyEnv)
		if !ok || strings.TrimSpace(rawKey) == "" {
			return jobConfig{}, fmt.Errorf("%s is required when %s=true",
				agentEmailOutboundDispatchPrivateKeyEnv, agentEmailOutboundEnabledEnv)
		}
		agentEmailOutboundClient.PrivateKey, err = agentemailoutbound.ParsePrivateKey(rawKey)
		if err != nil {
			return jobConfig{}, fmt.Errorf("%s: %w", agentEmailOutboundDispatchPrivateKeyEnv, err)
		}
		if err := agentEmailOutboundClient.Validate(); err != nil {
			return jobConfig{}, fmt.Errorf(
				"%s/%s/%s outbound agent-email dispatch configuration: %w",
				agentEmailOutboundDispatchEndpointEnv,
				agentEmailOutboundDispatchAudienceEnv,
				agentEmailOutboundDispatchKeyIDEnv,
				err,
			)
		}
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
	accountPurgeEnabled, err := boolEnv(lookup, accountPurgeEnabledEnv, false)
	if err != nil {
		return jobConfig{}, err
	}
	accountPurge := store.DefaultAccountPurgeWorkerConfig()
	if raw, ok := lookup(accountPurgeModeEnv); ok {
		accountPurge.Mode = store.AccountPurgeMode(
			strings.ToLower(strings.TrimSpace(raw)),
		)
	}
	if raw, ok := lookup(accountPurgeBatchSizeEnv); ok {
		accountPurge.BatchSize, err = parseIntEnv(accountPurgeBatchSizeEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(accountPurgeIntervalEnv); ok {
		accountPurge.Interval, err = parseDurationEnv(accountPurgeIntervalEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(accountPurgeBatchTimeoutEnv); ok {
		accountPurge.BatchTimeout, err = parseDurationEnv(accountPurgeBatchTimeoutEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if raw, ok := lookup(accountPurgeGraceEnv); ok {
		accountPurge.Grace, err = parseDurationEnv(accountPurgeGraceEnv, raw)
		if err != nil {
			return jobConfig{}, err
		}
	}
	if err := accountPurge.Validate(); err != nil {
		return jobConfig{}, fmt.Errorf(
			"%s/%s/%s/%s/%s account purge configuration: %w",
			accountPurgeModeEnv,
			accountPurgeBatchSizeEnv,
			accountPurgeIntervalEnv,
			accountPurgeBatchTimeoutEnv,
			accountPurgeGraceEnv,
			err,
		)
	}
	return jobConfig{
		avatarEnabled:                      avatarEnabled,
		avatar:                             avatar,
		messageRateBucketCleanupEnabled:    messageRateBucketCleanupEnabled,
		messageRateBucketCleanup:           messageRateBucketCleanup,
		agentEmailRateBucketCleanupEnabled: agentEmailRateBucketCleanupEnabled,
		agentEmailRateBucketCleanup:        agentEmailRateBucketCleanup,
		agentEmailOutboundEnabled:          agentEmailOutboundEnabled,
		agentEmailOutbound:                 agentEmailOutbound,
		agentEmailOutboundClient:           agentEmailOutboundClient,
		retentionEnabled:                   retentionEnabled,
		retention:                          retention,
		messageRetentionEnabled:            messageRetentionEnabled,
		messageRetention:                   messageRetention,
		agentEmailRetentionEnabled:         agentEmailRetentionEnabled,
		agentEmailRetention:                agentEmailRetention,
		accountPurgeEnabled:                accountPurgeEnabled,
		accountPurge:                       accountPurge,
	}, nil
}

func messageRateBucketCleanupMetricResult(deleted int64) worker.RetentionResult {
	if deleted == 0 {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func agentEmailRateBucketCleanupMetricResult(deleted int64) worker.RetentionResult {
	if deleted == 0 {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func agentEmailOutboundMetricResult(result agentEmailOutboundBatchResult) worker.RetentionResult {
	if result.empty() {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func agentEmailOutboundMetricCounts(
	result agentEmailOutboundBatchResult,
) worker.AgentEmailOutboundCounts {
	return worker.AgentEmailOutboundCounts{
		Claimed:           result.Claimed,
		Accepted:          result.Accepted,
		Delivered:         result.Delivered,
		Retried:           result.Retried,
		Bounced:           result.Bounced,
		Rejected:          result.Rejected,
		Failed:            result.Failed,
		Ambiguous:         result.Ambiguous,
		Canceled:          result.Canceled,
		ExpiredReconciled: result.ExpiredReconciled,
	}
}

func logAgentEmailOutboundResult(result agentEmailOutboundBatchResult) {
	if result.empty() {
		return
	}
	fmt.Fprintf(
		os.Stderr,
		"witself-worker: outbound agent email: claimed=%d accepted=%d delivered=%d retried=%d bounced=%d rejected=%d failed=%d ambiguous=%d canceled=%d expired_reconciled=%d\n",
		result.Claimed,
		result.Accepted,
		result.Delivered,
		result.Retried,
		result.Bounced,
		result.Rejected,
		result.Failed,
		result.Ambiguous,
		result.Canceled,
		result.ExpiredReconciled,
	)
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
		result.DeletedCanaryProofs == 0 &&
		result.ScannedOutbound == 0 &&
		result.EligibleOutbound == 0 &&
		result.DeletedOutbound == 0 &&
		result.DeletedOutboundBytes == 0 &&
		result.ScannedProviderEvents == 0 &&
		result.DeletedProviderEvents == 0 &&
		result.ScannedSuppressions == 0 &&
		result.EligibleSuppressions == 0 &&
		result.DeletedSuppressions == 0 {
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
		ScannedOutbound:       result.ScannedOutbound,
		EligibleOutbound:      result.EligibleOutbound,
		DeletedOutbound:       result.DeletedOutbound,
		DeletedOutboundBytes:  result.DeletedOutboundBytes,
		ScannedProviderEvents: result.ScannedProviderEvents,
		DeletedProviderEvents: result.DeletedProviderEvents,
		ScannedSuppressions:   result.ScannedSuppressions,
		EligibleSuppressions:  result.EligibleSuppressions,
		DeletedSuppressions:   result.DeletedSuppressions,
		ScanCapped:            result.ScanCapped,
	}
}

func accountPurgeMetricResult(result store.AccountPurgeBatchResult) worker.RetentionResult {
	if result.Scanned == 0 &&
		result.SkippedLocked == 0 &&
		result.Eligible == 0 &&
		result.PurgedAccounts == 0 &&
		result.DeferredVaultLifecycle == 0 &&
		result.AttachmentInvariantFailures == 0 &&
		result.ProvisionReceiptScrubs == 0 &&
		accountPurgeDeletedRows(result) == 0 {
		return worker.RetentionResultNoWork
	}
	return worker.RetentionResultSuccess
}

func accountPurgeMetricCounts(result store.AccountPurgeBatchResult) worker.AccountPurgeCounts {
	return worker.AccountPurgeCounts{
		Scanned:                     result.Scanned,
		SkippedLocked:               result.SkippedLocked,
		Eligible:                    result.Eligible,
		PurgedAccounts:              result.PurgedAccounts,
		DeletedRows:                 accountPurgeDeletedRows(result),
		DeferredVaultLifecycle:      result.DeferredVaultLifecycle,
		AttachmentInvariantFailures: result.AttachmentInvariantFailures,
		ProvisionReceiptScrubs:      result.ProvisionReceiptScrubs,
	}
}

func accountPurgeDeletedRows(result store.AccountPurgeBatchResult) int64 {
	var deletedRows int64
	for _, count := range result.DeletedByTable {
		if count > 0 {
			deletedRows += count
		}
	}
	return deletedRows
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
		"witself-worker: agent-email retention: mode=%s scanned=%d skipped_locked=%d scan_capped=%t eligible=%d deleted=%d deleted_raw_bytes=%d deferred_active=%d deferred_locked=%d deferred_oversize=%d deferred_budget=%d cleared_duplicate_links=%d deleted_canary_proofs=%d scanned_outbound=%d eligible_outbound=%d deleted_outbound=%d deleted_outbound_bytes=%d scanned_provider_events=%d deleted_provider_events=%d scanned_suppressions=%d eligible_suppressions=%d deleted_suppressions=%d\n",
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
		result.ScannedOutbound,
		result.EligibleOutbound,
		result.DeletedOutbound,
		result.DeletedOutboundBytes,
		result.ScannedProviderEvents,
		result.DeletedProviderEvents,
		result.ScannedSuppressions,
		result.EligibleSuppressions,
		result.DeletedSuppressions,
	)
}

func logAccountPurgeResult(mode store.AccountPurgeMode, result store.AccountPurgeBatchResult) {
	if accountPurgeMetricResult(result) == worker.RetentionResultNoWork {
		return
	}
	fmt.Fprintf(
		os.Stderr,
		"witself-worker: account purge: mode=%s scanned=%d skipped_locked=%d eligible=%d purged_accounts=%d deleted_rows=%d deferred_vault_lifecycle=%d attachment_invariant_failures=%d provision_receipt_scrubs=%d\n",
		mode,
		result.Scanned,
		result.SkippedLocked,
		result.Eligible,
		result.PurgedAccounts,
		accountPurgeDeletedRows(result),
		result.DeferredVaultLifecycle,
		result.AttachmentInvariantFailures,
		result.ProvisionReceiptScrubs,
	)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "witself-worker — durable Witself cell background jobs")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  witself-worker version    Print version information")
	_, _ = fmt.Fprintln(w, "  witself-worker serve      Run jobs, health, and metrics listeners")
	_, _ = fmt.Fprintln(w, "  witself-worker agent-email receipt-replay --account-id ID --send-id ID --expected-accepted-at TIME --expected-attempt-count 1 --json")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Required:")
	_, _ = fmt.Fprintln(w, "  WITSELF_DATABASE_URL  Postgres DSN (falls back to DATABASE_URL)")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Listeners:")
	_, _ = fmt.Fprintln(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	_, _ = fmt.Fprintln(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
}
