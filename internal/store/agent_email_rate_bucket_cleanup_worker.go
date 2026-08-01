package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	defaultAgentEmailRateBucketCleanupBatchSize    = maxAgentEmailRateCleanupBatch
	maxAgentEmailRateBucketCleanupWorkerBatchSize  = maxAgentEmailRateCleanupBatch
	defaultAgentEmailRateBucketCleanupInterval     = time.Minute
	minAgentEmailRateBucketCleanupInterval         = time.Minute
	maxAgentEmailRateBucketCleanupInterval         = 24 * time.Hour
	defaultAgentEmailRateBucketCleanupBatchTimeout = 10 * time.Second
	minAgentEmailRateBucketCleanupBatchTimeout     = time.Second
	maxAgentEmailRateBucketCleanupBatchTimeout     = 5 * time.Minute
)

// AgentEmailRateBucketCleanupWorkerConfig bounds one cleanup sweep and its
// cadence. A sweep drains consecutive fixed-size batches until it catches up
// or reaches BatchTimeout. Each replica may run this loop because
// DeleteStaleAgentEmailRateBuckets uses PostgreSQL row locks with SKIP LOCKED
// to divide a batch safely.
type AgentEmailRateBucketCleanupWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
}

// DefaultAgentEmailRateBucketCleanupWorkerConfig returns conservative
// production defaults for the general-purpose cell worker.
func DefaultAgentEmailRateBucketCleanupWorkerConfig() AgentEmailRateBucketCleanupWorkerConfig {
	return AgentEmailRateBucketCleanupWorkerConfig{
		BatchSize:    defaultAgentEmailRateBucketCleanupBatchSize,
		Interval:     defaultAgentEmailRateBucketCleanupInterval,
		BatchTimeout: defaultAgentEmailRateBucketCleanupBatchTimeout,
	}
}

// Validate rejects unbounded batches, busy loops, and ineffective deadlines.
func (c AgentEmailRateBucketCleanupWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxAgentEmailRateBucketCleanupWorkerBatchSize {
		return fmt.Errorf(
			"agent-email rate bucket cleanup batch size must be between 1 and %d",
			maxAgentEmailRateBucketCleanupWorkerBatchSize,
		)
	}
	if c.Interval < minAgentEmailRateBucketCleanupInterval ||
		c.Interval > maxAgentEmailRateBucketCleanupInterval {
		return fmt.Errorf(
			"agent-email rate bucket cleanup interval must be between %s and %s",
			minAgentEmailRateBucketCleanupInterval,
			maxAgentEmailRateBucketCleanupInterval,
		)
	}
	if c.BatchTimeout < minAgentEmailRateBucketCleanupBatchTimeout ||
		c.BatchTimeout > maxAgentEmailRateBucketCleanupBatchTimeout {
		return fmt.Errorf(
			"agent-email rate bucket cleanup batch timeout must be between %s and %s",
			minAgentEmailRateBucketCleanupBatchTimeout,
			maxAgentEmailRateBucketCleanupBatchTimeout,
		)
	}
	return nil
}

type agentEmailRateBucketCleanupDeleteFunc func(
	context.Context,
	time.Time,
	int,
) (int64, error)

type agentEmailRateBucketCleanupWaitFunc func(context.Context, time.Duration) bool

// RunAgentEmailRateBucketCleanupWorker runs one bounded sweep immediately and
// then one per interval until ctx is cancelled. A full batch triggers another
// batch in the same sweep so allowed sender rotation cannot outpace a fixed
// one-batch-per-interval drain. Recoverable errors are reported and retried on
// the next interval. Concurrent worker replicas divide rows through the cleanup
// statement's FOR UPDATE SKIP LOCKED selection.
func (s *Store) RunAgentEmailRateBucketCleanupWorker(
	ctx context.Context,
	cfg AgentEmailRateBucketCleanupWorkerConfig,
	onResult func(deleted int64),
	onError func(error),
) error {
	return runAgentEmailRateBucketCleanupWorker(
		ctx,
		cfg,
		time.Now,
		s.DeleteStaleAgentEmailRateBuckets,
		waitForAgentEmailRateBucketCleanupInterval,
		onResult,
		onError,
	)
}

func runAgentEmailRateBucketCleanupWorker(
	ctx context.Context,
	cfg AgentEmailRateBucketCleanupWorkerConfig,
	now func() time.Time,
	deleteBatch agentEmailRateBucketCleanupDeleteFunc,
	wait agentEmailRateBucketCleanupWaitFunc,
	onResult func(deleted int64),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if now == nil || deleteBatch == nil || wait == nil {
		return errors.New("agent-email rate bucket cleanup worker dependencies are required")
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, cfg.BatchTimeout)
		before := now().UTC()
		for {
			deleted, err := deleteBatch(attemptCtx, before, cfg.BatchSize)
			if err != nil {
				if ctx.Err() != nil {
					cancelAttempt()
					return nil
				}
				if !errors.Is(err, context.Canceled) && onError != nil {
					onError(err)
				}
				break
			}
			if onResult != nil {
				onResult(deleted)
			}
			if deleted < int64(cfg.BatchSize) {
				break
			}
			if err := attemptCtx.Err(); err != nil {
				if ctx.Err() != nil {
					cancelAttempt()
					return nil
				}
				if onError != nil {
					onError(err)
				}
				break
			}
		}
		cancelAttempt()

		if !wait(ctx, cfg.Interval) {
			return nil
		}
	}
}

func waitForAgentEmailRateBucketCleanupInterval(
	ctx context.Context,
	interval time.Duration,
) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
