package messagerunner

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultFailureBackoff is the first transient-failure retry delay.
	DefaultFailureBackoff = time.Second
	// DefaultMaximumBackoff caps the transient-failure retry delay.
	DefaultMaximumBackoff = time.Minute
	// DefaultMaximumFailures means retry until caller cancellation.
	DefaultMaximumFailures = 0
)

// LoopOptions controls only local retry behavior. A zero MaximumFailures means
// retry transient failures until the caller cancels the service context.
type LoopOptions struct {
	FailureBackoff  time.Duration
	MaximumBackoff  time.Duration
	MaximumFailures int
	Observe         func(RunResult, error)
}

// Serve continuously executes bounded runner cycles. Long polling happens in
// RunOnce; transient API/provider errors use capped exponential backoff, while
// an invalid local configuration or rebound token fails closed immediately.
func (r Runner) Serve(ctx context.Context, options LoopOptions) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidConfiguration)
	}
	if options.FailureBackoff == 0 {
		options.FailureBackoff = DefaultFailureBackoff
	}
	if options.MaximumBackoff == 0 {
		options.MaximumBackoff = DefaultMaximumBackoff
	}
	if options.FailureBackoff < time.Millisecond || options.MaximumBackoff < options.FailureBackoff || options.MaximumFailures < 0 {
		return fmt.Errorf("%w: retry policy is invalid", ErrInvalidConfiguration)
	}

	backoff := options.FailureBackoff
	failures := 0
	for {
		result, err := r.RunOnce(ctx)
		// A service stop cancels the in-flight long poll. That is a normal
		// lifecycle transition, not a failed processing cycle, so do not poison
		// the content-free health record with an artificial cancellation error.
		if options.Observe != nil && (err == nil || ctx.Err() == nil) {
			options.Observe(result, err)
		}
		if err == nil {
			failures = 0
			backoff = options.FailureBackoff
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrInvalidConfiguration) || errors.Is(err, ErrIdentityMismatch) {
			return err
		}
		failures++
		if options.MaximumFailures != DefaultMaximumFailures && failures >= options.MaximumFailures {
			return fmt.Errorf("message runner stopped after %d consecutive failures: %w", failures, err)
		}
		if err := waitForRetry(ctx, backoff); err != nil {
			return nil
		}
		backoff = min(backoff*2, options.MaximumBackoff)
	}
}

func waitForRetry(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
