package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMessageRateBucketCleanupWorkerConfigBounds(t *testing.T) {
	defaults := DefaultMessageRateBucketCleanupWorkerConfig()
	if defaults.BatchSize != 10_000 ||
		defaults.Interval != time.Minute ||
		defaults.BatchTimeout != 10*time.Second {
		t.Fatalf("defaults = %#v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}

	for _, invalid := range []MessageRateBucketCleanupWorkerConfig{
		{BatchSize: 0, Interval: time.Minute, BatchTimeout: time.Second},
		{BatchSize: maxMessageRateBucketCleanupWorkerBatchSize + 1, Interval: time.Minute, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: minMessageRateBucketCleanupInterval - time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: maxMessageRateBucketCleanupInterval + time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: minMessageRateBucketCleanupBatchTimeout - time.Nanosecond},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: maxMessageRateBucketCleanupBatchTimeout + time.Nanosecond},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}

func TestRunMessageRateBucketCleanupWorkerRetriesAndReportsBoundedBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultMessageRateBucketCleanupWorkerConfig()
	fixedNow := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.FixedZone("test", -6*60*60))
	transientErr := errors.New("transient database failure")
	var calls, waits int
	var results []int64
	var reportedErrors []error

	err := runMessageRateBucketCleanupWorker(
		ctx,
		cfg,
		func() time.Time { return fixedNow },
		func(attemptCtx context.Context, before time.Time, limit int) MessageRateBucketCleanupBatchResult {
			calls++
			if before != fixedNow.UTC() {
				t.Fatalf("cleanup cutoff = %s, want %s", before, fixedNow.UTC())
			}
			if limit != cfg.BatchSize {
				t.Fatalf("cleanup limit = %d, want %d", limit, cfg.BatchSize)
			}
			deadline, ok := attemptCtx.Deadline()
			if !ok || time.Until(deadline) > cfg.BatchTimeout {
				t.Fatalf("attempt deadline = %s, configured timeout %s", deadline, cfg.BatchTimeout)
			}
			switch calls {
			case 1:
				return MessageRateBucketCleanupBatchResult{RateBucketError: transientErr}
			case 2:
				return MessageRateBucketCleanupBatchResult{}
			case 3:
				return MessageRateBucketCleanupBatchResult{Deleted: 7}
			default:
				t.Fatalf("unexpected cleanup call %d", calls)
				return MessageRateBucketCleanupBatchResult{}
			}
		},
		func(waitCtx context.Context, interval time.Duration) bool {
			waits++
			if interval != cfg.Interval {
				t.Fatalf("wait interval = %s, want %s", interval, cfg.Interval)
			}
			if waits == 3 {
				cancel()
				return false
			}
			return waitCtx.Err() == nil
		},
		func(result MessageRateBucketCleanupBatchResult) {
			if result.RateBucketError != nil {
				reportedErrors = append(reportedErrors, result.RateBucketError)
			} else {
				results = append(results, result.Deleted)
			}
		},
	)
	if err != nil {
		t.Fatalf("worker returned error: %v", err)
	}
	if calls != 3 || waits != 3 {
		t.Fatalf("calls/waits = %d/%d, want 3/3", calls, waits)
	}
	if !reflect.DeepEqual(results, []int64{0, 7}) {
		t.Fatalf("results = %#v, want [0 7]", results)
	}
	if len(reportedErrors) != 1 || !errors.Is(reportedErrors[0], transientErr) {
		t.Fatalf("reported errors = %#v", reportedErrors)
	}
}

func TestRunMessageRateBucketCleanupWorkerHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	var resultCalls int

	go func() {
		done <- runMessageRateBucketCleanupWorker(
			ctx,
			DefaultMessageRateBucketCleanupWorkerConfig(),
			time.Now,
			func(attemptCtx context.Context, _ time.Time, _ int) MessageRateBucketCleanupBatchResult {
				close(started)
				<-attemptCtx.Done()
				return MessageRateBucketCleanupBatchResult{RateBucketError: attemptCtx.Err()}
			},
			waitForMessageRateBucketCleanupInterval,
			func(MessageRateBucketCleanupBatchResult) { resultCalls++ },
		)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("immediate cleanup batch did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled worker returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if resultCalls != 0 {
		t.Fatalf("callbacks after cancellation = %d", resultCalls)
	}
}

func TestMessageRateBucketCleanupActivityRetentionResult(t *testing.T) {
	cause := errors.New("synthetic retention query failure")
	for _, stop := range []string{"none", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var order []string
			result := messageRateBucketCleanupBatch(ctx, time.Now(), 10000,
				func(context.Context, time.Time, int) (int64, error) {
					order = append(order, "buckets")
					return 7, nil
				},
				func(batchCtx context.Context, _ time.Time, limit int) (int64, error) {
					order = append(order, "activity")
					if limit != 1000 {
						t.Fatalf("activity batch limit = %d", limit)
					}
					if stop == "cancel" {
						cancel()
					}
					return 0, cause
				})
			if result.Deleted != 7 || result.RateBucketError != nil || !reflect.DeepEqual(order, []string{"buckets", "activity"}) {
				t.Fatalf("rate-bucket success lost: %+v, order %v", result, order)
			}
			if stop == "cancel" {
				if result.ActivityRetentionError != nil {
					t.Fatalf("cancelled retention reported failure: %v", result.ActivityRetentionError)
				}
			} else if !errors.Is(result.ActivityRetentionError, cause) || result.ActivityRetentionError.Error() != "activity event retention: "+cause.Error() {
				t.Fatalf("retention cause not wrapped: %v", result.ActivityRetentionError)
			}
		})
	}
}

func TestMessageRateBucketCleanupActivityRetentionDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	result := messageRateBucketCleanupBatch(ctx, time.Now(), 1,
		func(context.Context, time.Time, int) (int64, error) { return 0, nil },
		func(ctx context.Context, _ time.Time, _ int) (int64, error) { return 0, ctx.Err() })
	if result.ActivityRetentionError != nil {
		t.Fatalf("expired retention batch reported failure: %v", result.ActivityRetentionError)
	}
}
