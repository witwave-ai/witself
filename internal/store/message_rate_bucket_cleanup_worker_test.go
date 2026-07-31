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
		func(attemptCtx context.Context, before time.Time, limit int) (int64, error) {
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
				return 0, transientErr
			case 2:
				return 0, nil
			case 3:
				return 7, nil
			default:
				t.Fatalf("unexpected cleanup call %d", calls)
				return 0, nil
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
		func(deleted int64) { results = append(results, deleted) },
		func(err error) { reportedErrors = append(reportedErrors, err) },
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
	var resultCalls, errorCalls int

	go func() {
		done <- runMessageRateBucketCleanupWorker(
			ctx,
			DefaultMessageRateBucketCleanupWorkerConfig(),
			time.Now,
			func(attemptCtx context.Context, _ time.Time, _ int) (int64, error) {
				close(started)
				<-attemptCtx.Done()
				return 0, attemptCtx.Err()
			},
			waitForMessageRateBucketCleanupInterval,
			func(int64) { resultCalls++ },
			func(error) { errorCalls++ },
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
	if resultCalls != 0 || errorCalls != 0 {
		t.Fatalf("callbacks after cancellation = result %d, error %d", resultCalls, errorCalls)
	}
}
