package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	defaultMessageRateBucketCleanupBatchSize    = maxMessageRateCleanupBatch
	maxMessageRateBucketCleanupWorkerBatchSize  = maxMessageRateCleanupBatch
	defaultMessageRateBucketCleanupInterval     = time.Minute
	minMessageRateBucketCleanupInterval         = time.Minute
	maxMessageRateBucketCleanupInterval         = 24 * time.Hour
	defaultMessageRateBucketCleanupBatchTimeout = 10 * time.Second
	minMessageRateBucketCleanupBatchTimeout     = time.Second
	maxMessageRateBucketCleanupBatchTimeout     = 5 * time.Minute
)

// MessageRateBucketCleanupWorkerConfig bounds one cleanup attempt and its
// cadence. Each replica may run this loop because DeleteStaleMessageRateBuckets
// uses PostgreSQL row locks with SKIP LOCKED to divide a batch safely.
type MessageRateBucketCleanupWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
}

// DefaultMessageRateBucketCleanupWorkerConfig returns conservative production
// defaults for the general-purpose cell worker.
func DefaultMessageRateBucketCleanupWorkerConfig() MessageRateBucketCleanupWorkerConfig {
	return MessageRateBucketCleanupWorkerConfig{
		BatchSize:    defaultMessageRateBucketCleanupBatchSize,
		Interval:     defaultMessageRateBucketCleanupInterval,
		BatchTimeout: defaultMessageRateBucketCleanupBatchTimeout,
	}
}

// Validate rejects unbounded batches, busy loops, and ineffective deadlines.
func (c MessageRateBucketCleanupWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxMessageRateBucketCleanupWorkerBatchSize {
		return fmt.Errorf(
			"message rate bucket cleanup batch size must be between 1 and %d",
			maxMessageRateBucketCleanupWorkerBatchSize,
		)
	}
	if c.Interval < minMessageRateBucketCleanupInterval ||
		c.Interval > maxMessageRateBucketCleanupInterval {
		return fmt.Errorf(
			"message rate bucket cleanup interval must be between %s and %s",
			minMessageRateBucketCleanupInterval,
			maxMessageRateBucketCleanupInterval,
		)
	}
	if c.BatchTimeout < minMessageRateBucketCleanupBatchTimeout ||
		c.BatchTimeout > maxMessageRateBucketCleanupBatchTimeout {
		return fmt.Errorf(
			"message rate bucket cleanup batch timeout must be between %s and %s",
			minMessageRateBucketCleanupBatchTimeout,
			maxMessageRateBucketCleanupBatchTimeout,
		)
	}
	return nil
}

type messageRateBucketCleanupDeleteFunc func(
	context.Context,
	time.Time,
	int,
) (int64, error)

type messageRateBucketCleanupWaitFunc func(context.Context, time.Duration) bool

// RunMessageRateBucketCleanupWorker runs one bounded batch immediately and
// then one per interval until ctx is cancelled. Recoverable batch errors are
// reported and retried on the next interval. Concurrent worker replicas divide
// rows through the cleanup statement's FOR UPDATE SKIP LOCKED selection.
func (s *Store) RunMessageRateBucketCleanupWorker(
	ctx context.Context,
	cfg MessageRateBucketCleanupWorkerConfig,
	onResult func(deleted int64),
	onError func(error),
) error {
	return runMessageRateBucketCleanupWorker(
		ctx,
		cfg,
		time.Now,
		s.DeleteStaleMessageRateBuckets,
		waitForMessageRateBucketCleanupInterval,
		onResult,
		onError,
	)
}

func runMessageRateBucketCleanupWorker(
	ctx context.Context,
	cfg MessageRateBucketCleanupWorkerConfig,
	now func() time.Time,
	deleteBatch messageRateBucketCleanupDeleteFunc,
	wait messageRateBucketCleanupWaitFunc,
	onResult func(deleted int64),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if now == nil || deleteBatch == nil || wait == nil {
		return errors.New("message rate bucket cleanup worker dependencies are required")
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, cfg.BatchTimeout)
		deleted, err := deleteBatch(attemptCtx, now().UTC(), cfg.BatchSize)
		cancelAttempt()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !errors.Is(err, context.Canceled) && onError != nil {
				onError(err)
			}
		} else if onResult != nil {
			onResult(deleted)
		}

		if !wait(ctx, cfg.Interval) {
			return nil
		}
	}
}

func waitForMessageRateBucketCleanupInterval(
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
