package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAgentEmailRateBucketCleanupWorkerConfigBounds(t *testing.T) {
	defaults := DefaultAgentEmailRateBucketCleanupWorkerConfig()
	if defaults.BatchSize != 10_000 ||
		defaults.Interval != time.Minute ||
		defaults.BatchTimeout != 10*time.Second {
		t.Fatalf("defaults = %#v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}

	for _, invalid := range []AgentEmailRateBucketCleanupWorkerConfig{
		{BatchSize: 0, Interval: time.Minute, BatchTimeout: time.Second},
		{BatchSize: maxAgentEmailRateBucketCleanupWorkerBatchSize + 1, Interval: time.Minute, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: minAgentEmailRateBucketCleanupInterval - time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: maxAgentEmailRateBucketCleanupInterval + time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: minAgentEmailRateBucketCleanupBatchTimeout - time.Nanosecond},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: maxAgentEmailRateBucketCleanupBatchTimeout + time.Nanosecond},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}

func TestRunAgentEmailRateBucketCleanupWorkerRetriesAndReportsBoundedBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultAgentEmailRateBucketCleanupWorkerConfig()
	fixedNow := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.FixedZone("test", -6*60*60))
	transientErr := errors.New("transient database failure")
	var calls, waits int
	var results []int64
	var reportedErrors []error

	err := runAgentEmailRateBucketCleanupWorker(
		ctx,
		cfg,
		func() time.Time { return fixedNow },
		[]agentEmailRateBucketCleanupDeleteFunc{func(attemptCtx context.Context, before time.Time, limit int) (int64, error) {
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
		}},
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

func TestRunAgentEmailRateBucketCleanupWorkerDrainsFullBatchesWithinOneSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultAgentEmailRateBucketCleanupWorkerConfig()
	cfg.BatchSize = 5
	fixedNow := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	deletions := []int64{5, 5, 2}
	var calls, waits int
	var results []int64

	err := runAgentEmailRateBucketCleanupWorker(
		ctx,
		cfg,
		func() time.Time { return fixedNow },
		[]agentEmailRateBucketCleanupDeleteFunc{func(attemptCtx context.Context, before time.Time, limit int) (int64, error) {
			if before != fixedNow || limit != cfg.BatchSize {
				t.Fatalf("cleanup call = before %s limit %d", before, limit)
			}
			if attemptCtx.Err() != nil {
				t.Fatalf("cleanup context ended before call %d: %v", calls+1, attemptCtx.Err())
			}
			deleted := deletions[calls]
			calls++
			return deleted, nil
		}},
		func(_ context.Context, interval time.Duration) bool {
			waits++
			if interval != cfg.Interval {
				t.Fatalf("wait interval = %s, want %s", interval, cfg.Interval)
			}
			cancel()
			return false
		},
		func(deleted int64) { results = append(results, deleted) },
		func(err error) { t.Fatalf("unexpected cleanup error: %v", err) },
	)
	if err != nil {
		t.Fatalf("worker returned error: %v", err)
	}
	if calls != 3 || waits != 1 {
		t.Fatalf("calls/waits = %d/%d, want 3/1", calls, waits)
	}
	if !reflect.DeepEqual(results, deletions) {
		t.Fatalf("results = %#v, want %#v", results, deletions)
	}
}

func TestRunAgentEmailRateBucketCleanupWorkerDrainsInboundAndOutboundTables(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultAgentEmailRateBucketCleanupWorkerConfig()
	cfg.BatchSize = 5
	fixedNow := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	deletions := [][]int64{{5, 1}, {5, 5, 2}}
	calls := make([]int, len(deletions))
	var results []int64
	var waits int
	deleteBatches := make([]agentEmailRateBucketCleanupDeleteFunc, len(deletions))
	for lane := range deletions {
		lane := lane
		deleteBatches[lane] = func(attemptCtx context.Context, before time.Time, limit int) (int64, error) {
			if before != fixedNow || limit != cfg.BatchSize || attemptCtx.Err() != nil {
				t.Fatalf("lane %d cleanup call = before %s limit %d error %v",
					lane, before, limit, attemptCtx.Err())
			}
			deleted := deletions[lane][calls[lane]]
			calls[lane]++
			return deleted, nil
		}
	}

	err := runAgentEmailRateBucketCleanupWorker(
		ctx,
		cfg,
		func() time.Time { return fixedNow },
		deleteBatches,
		func(_ context.Context, interval time.Duration) bool {
			waits++
			if interval != cfg.Interval {
				t.Fatalf("wait interval = %s, want %s", interval, cfg.Interval)
			}
			cancel()
			return false
		},
		func(deleted int64) { results = append(results, deleted) },
		func(err error) { t.Fatalf("unexpected cleanup error: %v", err) },
	)
	if err != nil {
		t.Fatalf("worker returned error: %v", err)
	}
	if !reflect.DeepEqual(calls, []int{2, 3}) || waits != 1 {
		t.Fatalf("calls/waits = %#v/%d, want [2 3]/1", calls, waits)
	}
	if !reflect.DeepEqual(results, []int64{5, 5, 1, 5, 2}) {
		t.Fatalf("results = %#v, want [5 5 1 5 2]", results)
	}
}

func TestRunAgentEmailRateBucketCleanupWorkerDoesNotStarveHealthyTable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultAgentEmailRateBucketCleanupWorkerConfig()
	transientErr := errors.New("inbound cleanup failed")
	var healthyCalls, waits int
	var results []int64
	var reportedErrors []error

	err := runAgentEmailRateBucketCleanupWorker(
		ctx,
		cfg,
		time.Now,
		[]agentEmailRateBucketCleanupDeleteFunc{
			func(context.Context, time.Time, int) (int64, error) {
				return 0, transientErr
			},
			func(context.Context, time.Time, int) (int64, error) {
				healthyCalls++
				return 3, nil
			},
		},
		func(context.Context, time.Duration) bool {
			waits++
			cancel()
			return false
		},
		func(deleted int64) { results = append(results, deleted) },
		func(err error) { reportedErrors = append(reportedErrors, err) },
	)
	if err != nil {
		t.Fatalf("worker returned error: %v", err)
	}
	if healthyCalls != 1 || waits != 1 || !reflect.DeepEqual(results, []int64{3}) {
		t.Fatalf("healthy calls/waits/results = %d/%d/%#v", healthyCalls, waits, results)
	}
	if len(reportedErrors) != 1 || !errors.Is(reportedErrors[0], transientErr) {
		t.Fatalf("reported errors = %#v", reportedErrors)
	}
}

func TestRunAgentEmailRateBucketCleanupWorkerHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	var resultCalls, errorCalls int

	go func() {
		done <- runAgentEmailRateBucketCleanupWorker(
			ctx,
			DefaultAgentEmailRateBucketCleanupWorkerConfig(),
			time.Now,
			[]agentEmailRateBucketCleanupDeleteFunc{func(attemptCtx context.Context, _ time.Time, _ int) (int64, error) {
				close(started)
				<-attemptCtx.Done()
				return 0, attemptCtx.Err()
			}},
			waitForAgentEmailRateBucketCleanupInterval,
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
