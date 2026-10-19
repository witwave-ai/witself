package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TranscriptStorageReconcileConfig bounds the cost and lifetime of each snapshot.
type TranscriptStorageReconcileConfig struct {
	Interval    time.Duration
	DutyCycle   float64
	PageEntries int
	UnitTimeout time.Duration
}

// DefaultTranscriptStorageReconcileConfig returns the bounded production defaults.
func DefaultTranscriptStorageReconcileConfig() TranscriptStorageReconcileConfig {
	return TranscriptStorageReconcileConfig{5 * time.Minute, .5, 1000, 240 * time.Second}
}

// Validate rejects settings outside the supported cost and lifetime bounds.
func (c TranscriptStorageReconcileConfig) Validate() error {
	switch {
	case c.Interval < 10*time.Second || c.Interval > time.Hour:
		return errors.New("transcript storage reconcile interval must be between 10s and 1h")
	case math.IsNaN(c.DutyCycle) || c.DutyCycle < .05 || c.DutyCycle > 1:
		return errors.New("transcript storage reconcile duty cycle must be between 0.05 and 1.0")
	case c.PageEntries < 1 || c.PageEntries > 10000:
		return errors.New("transcript storage reconcile page entries must be between 1 and 10000")
	case c.UnitTimeout < 10*time.Second || c.UnitTimeout > 280*time.Second:
		return errors.New("transcript storage reconcile unit timeout must be between 10s and 280s")
	}
	return nil
}

// TranscriptStorageReconcileStatus reports value-free eligible, deferred and stuck counts.
type TranscriptStorageReconcileStatus struct {
	UnverifiedEligible, UnverifiedDeferred int64
	Stuck                                  int64
}

// TranscriptStorageReconcilePassResult summarizes one pass without account identifiers.
type TranscriptStorageReconcilePassResult struct {
	Corrected, Confirmed, Raced, Busy, Anomaly, Timeout, Error, Skipped int64
	Entries                                                             int64
	LeaseHeld                                                           bool
	UnverifiedEligible, UnverifiedDeferred                              int64
	Stuck                                                               int64
	MaxUnitElapsed, Elapsed                                             time.Duration
	Err                                                                 error
	nextDue                                                             time.Time
	terminal                                                            bool
}

// TranscriptStorageReconcileSummary aggregates run-until-done outcomes and final status.
type TranscriptStorageReconcileSummary struct {
	Passes, LeaseHeldPasses                                             int
	Corrected, Confirmed, Raced, Busy, Anomaly, Timeout, Error, Skipped int64
	Entries                                                             int64
	MaxUnitElapsed, Elapsed                                             time.Duration
	UnverifiedEligible, UnverifiedDeferred                              int64
}

// TranscriptStorageVerification reports independent recount mismatches and exactness.
type TranscriptStorageVerification struct {
	Mismatches int
	Exact      bool
}

type transcriptStorageReconcileUnit struct {
	accountID string
	shard     int16
}

type transcriptStorageReconcileMeasurement struct {
	counter, epoch, entries, bytes, delta int64
}

type transcriptStorageReconcileHooks struct {
	afterCounterRead, afterPage, beforeApply, afterApply func(context.Context, int32) error
	applyLockTimeout                                     *time.Duration
	measureIdleTimeout                                   *time.Duration
	backoff                                              *time.Duration
}

// Tests install hooks only while no other runner uses them. Production leaves nil.
var transcriptStorageReconcileTestHooks *transcriptStorageReconcileHooks

type transcriptStorageReconcileLease struct {
	conn          *pgxpool.Conn
	pid           int32
	held, discard bool
}

type transcriptStorageReconcileBackoff struct {
	until                            time.Time
	failures, hardFailures, timeouts int
}

type transcriptStorageReconcileRunner struct {
	backoff map[transcriptStorageReconcileUnit]transcriptStorageReconcileBackoff
	cursor  transcriptStorageReconcileUnit
}

func newTranscriptStorageReconcileRunner() *transcriptStorageReconcileRunner {
	return &transcriptStorageReconcileRunner{backoff: make(map[transcriptStorageReconcileUnit]transcriptStorageReconcileBackoff), cursor: transcriptStorageReconcileUnit{shard: -1}}
}

func (r *transcriptStorageReconcileRunner) stuck() int64 {
	var n int64
	for _, b := range r.backoff {
		if b.timeouts >= 3 {
			n++
		}
	}
	return n
}

func (r *transcriptStorageReconcileRunner) prune(seen map[transcriptStorageReconcileUnit]bool) {
	for unit := range r.backoff {
		if !seen[unit] {
			delete(r.backoff, unit)
		}
	}
}

func (r *transcriptStorageReconcileRunner) record(u transcriptStorageReconcileUnit, outcome string, now time.Time) bool {
	if outcome == "corrected" || outcome == "confirmed" || outcome == "skipped" {
		delete(r.backoff, u)
		return false
	}
	b := r.backoff[u]
	if outcome == "raced" {
		b.failures = 0
	} else {
		b.failures++
	}
	if outcome == "timeout" {
		b.timeouts++
	} else {
		b.timeouts = 0
	}
	if outcome == "timeout" || outcome == "error" {
		b.hardFailures++
	} else {
		b.hardFailures = 0
	}
	delay := 30 * time.Second
	if outcome != "raced" {
		delay = time.Minute * time.Duration(1<<min(b.failures-1, 5))
		delay = min(delay, 30*time.Minute)
	}
	if h := transcriptStorageReconcileTestHooks; h != nil && h.backoff != nil {
		delay = *h.backoff
	}
	b.until = now.Add(delay)
	r.backoff[u] = b
	return outcome == "anomaly" || b.hardFailures >= 3
}

func (p *TranscriptStorageReconcilePassResult) add(outcome string) {
	switch outcome {
	case "corrected":
		p.Corrected++
	case "confirmed":
		p.Confirmed++
	case "raced":
		p.Raced++
	case "busy":
		p.Busy++
	case "anomaly":
		p.Anomaly++
	case "timeout":
		p.Timeout++
	case "error":
		p.Error++
	case "skipped":
		p.Skipped++
	}
}

func (p TranscriptStorageReconcilePassResult) units() int64 {
	return p.Corrected + p.Confirmed + p.Raced + p.Busy + p.Anomaly + p.Timeout + p.Error + p.Skipped
}

func transcriptStorageReconcileErrorResult(err error) string {
	var pgerr *pgconn.PgError
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgerr) && pgerr.Code == "57014") {
		return "timeout"
	}
	return "error"
}

func transcriptStorageReconcileHook(ctx context.Context, pid int32, hook func(context.Context, int32) error) error {
	if hook == nil {
		return nil
	}
	return hook(ctx, pid)
}

// A session seam keeps the pass scheduler independent of PostgreSQL for tests.
// The production implementation still uses one pinned connection for every call.
type transcriptStorageReconcilePassSession interface {
	status(context.Context) (TranscriptStorageReconcileStatus, error)
	pick(context.Context, transcriptStorageReconcileUnit) ([]transcriptStorageReconcileUnit, error)
	measure(context.Context, TranscriptStorageReconcileConfig, transcriptStorageReconcileUnit) (transcriptStorageReconcileMeasurement, string, error)
	apply(context.Context, transcriptStorageReconcileUnit, transcriptStorageReconcileMeasurement) (string, error)
	invalidate()
	invalid() bool
}

func (l *transcriptStorageReconcileLease) invalidate()   { l.discard = true }
func (l *transcriptStorageReconcileLease) invalid() bool { return l.discard }

func (s *Store) reconcileTranscriptStorageUsagePass(ctx context.Context, cfg TranscriptStorageReconcileConfig, runner *transcriptStorageReconcileRunner, onStatus func(TranscriptStorageReconcileStatus)) (result TranscriptStorageReconcilePassResult) {
	start := time.Now()
	defer func() { result.Elapsed = time.Since(start); result.Stuck = runner.stuck() }()
	passCtx, cancel := context.WithTimeout(ctx, cfg.Interval)
	defer cancel()
	lease, held, err := s.acquireTranscriptStorageReconcileLease(passCtx)
	if err != nil {
		result.Err = errors.New("transcript storage reconcile lease unavailable")
		return
	}
	defer func() {
		// Discard/close first: a replacement status connection must also work
		// with a pool of one, after a timed-out or cancelled unit.
		finishTranscriptStorageReconcilePass(ctx, &result, runner, held && lease.invalid(), lease.close, s.TranscriptStorageReconcileStatus, onStatus)
	}()
	return runTranscriptStorageReconcilePass(ctx, passCtx, cfg, runner, lease, held, time.Now, onStatus)
}

func finishTranscriptStorageReconcilePass(ctx context.Context, result *TranscriptStorageReconcilePassResult, runner *transcriptStorageReconcileRunner, needsStatus bool, closeFn func(), statusFn func(context.Context) (TranscriptStorageReconcileStatus, error), onStatus func(TranscriptStorageReconcileStatus)) {
	closeFn()
	if !needsStatus {
		return
	}
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	status, err := statusFn(statusCtx)
	if err != nil {
		if result.Err == nil {
			result.Err = errors.New("transcript storage reconcile status unavailable")
		}
		return
	}
	status.Stuck = runner.stuck()
	result.UnverifiedEligible = status.UnverifiedEligible
	result.UnverifiedDeferred = status.UnverifiedDeferred
	if onStatus != nil {
		onStatus(status)
	}
}

// Interval bounds the initial status, picks, and when a pass starts units. An admitted unit
// has its own UnitTimeout under the caller's context. Lease cleanup and a fresh
// final status after the pass deadline use their separate bounded contexts.
func runTranscriptStorageReconcilePass(ctx, passCtx context.Context, cfg TranscriptStorageReconcileConfig, runner *transcriptStorageReconcileRunner, lease transcriptStorageReconcilePassSession, held bool, now func() time.Time, onStatus func(TranscriptStorageReconcileStatus)) (result TranscriptStorageReconcilePassResult) {
	start := now()
	passCtx, cancelPass := context.WithTimeout(passCtx, cfg.Interval)
	defer cancelPass()
	defer func() { result.Elapsed = now().Sub(start); result.Stuck = runner.stuck() }()
	publish := func(statusCtx context.Context) bool {
		status, statusErr := lease.status(statusCtx)
		if statusErr != nil {
			lease.invalidate()
			result.Err = errors.New("transcript storage reconcile status unavailable")
			return false
		}
		status.Stuck = runner.stuck()
		result.UnverifiedEligible = status.UnverifiedEligible
		result.UnverifiedDeferred = status.UnverifiedDeferred
		if onStatus != nil {
			onStatus(status)
		}
		return true
	}
	result.LeaseHeld = !held
	if !publish(passCtx) || !held {
		return
	}
	defer func() {
		if !lease.invalid() {
			statusCtx, cancelStatus := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelStatus()
			publish(statusCtx)
		}
	}()
	seen := make(map[transcriptStorageReconcileUnit]bool)
	wrapped := false
	for passCtx.Err() == nil && now().Sub(start) < cfg.Interval {
		pickStart := now()
		candidates, pickErr := lease.pick(passCtx, runner.cursor)
		pickElapsed := now().Sub(pickStart)
		if pickErr != nil {
			lease.invalidate()
			if passCtx.Err() != nil && ctx.Err() == nil {
				// Interval stopped admission; discard the cancelled connection
				// so finish reads status through a fresh one.
				return
			}
			result.Err = errors.New("transcript storage reconcile pick failed")
			return
		}
		if passCtx.Err() != nil || now().Sub(start) >= cfg.Interval {
			return
		}
		if len(candidates) == 0 {
			if wrapped {
				runner.prune(seen)
				break
			}
			runner.cursor = transcriptStorageReconcileUnit{shard: -1}
			wrapped = true
			continue
		}
		for _, unit := range candidates {
			if now().Sub(start) >= cfg.Interval || passCtx.Err() != nil {
				return
			}
			runner.cursor = unit
			if seen[unit] {
				runner.prune(seen)
				return
			}
			seen[unit] = true
			if b, ok := runner.backoff[unit]; ok && now().Before(b.until) {
				if result.nextDue.IsZero() || b.until.Before(result.nextDue) {
					result.nextDue = b.until
				}
				continue
			}
			unitStart := now()
			unitCtx, cancel := context.WithTimeout(ctx, cfg.UnitTimeout)
			measured, outcome, unitErr := lease.measure(unitCtx, cfg, unit)
			result.Entries += measured.entries
			if unitErr == nil && outcome == "" {
				outcome, unitErr = lease.apply(unitCtx, unit, measured)
			}
			if unitErr != nil {
				outcome = transcriptStorageReconcileErrorResult(unitErr)
			}
			if lease.invalid() && unitErr != nil && unitCtx.Err() == nil {
				result.Err = errors.New("transcript storage reconcile lease lost")
			}
			if unitCtx.Err() != nil || outcome == "timeout" {
				lease.invalidate()
			}
			cancel()
			result.MaxUnitElapsed = max(result.MaxUnitElapsed, pickElapsed+now().Sub(unitStart))
			result.add(outcome)
			result.terminal = runner.record(unit, outcome, now()) || result.terminal
			if b, ok := runner.backoff[unit]; ok && (result.nextDue.IsZero() || b.until.Before(result.nextDue)) {
				result.nextDue = b.until
			}
			if lease.invalid() || result.terminal {
				return
			}
		}
	}
	return
}

// RunTranscriptStorageReconcileWorker owns one runner's backoff for its lifetime.
// Status is delivered immediately, including by replicas that cannot take the lease.
func (s *Store) RunTranscriptStorageReconcileWorker(ctx context.Context, cfg TranscriptStorageReconcileConfig, onResult func(TranscriptStorageReconcilePassResult), onStatus func(TranscriptStorageReconcileStatus)) error {
	runner := newTranscriptStorageReconcileRunner()
	return runTranscriptStorageReconcileWorker(ctx, cfg, func(ctx context.Context) TranscriptStorageReconcilePassResult {
		return s.reconcileTranscriptStorageUsagePass(ctx, cfg, runner, onStatus)
	}, waitForMessageRateBucketCleanupInterval, onResult)
}

func runTranscriptStorageReconcileWorker(ctx context.Context, cfg TranscriptStorageReconcileConfig, passFn func(context.Context) TranscriptStorageReconcilePassResult, waitFn func(context.Context, time.Duration) bool, onResult func(TranscriptStorageReconcilePassResult)) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	for ctx.Err() == nil {
		result := passFn(ctx)
		if onResult != nil {
			onResult(result)
		}
		delay := cfg.Interval
		if result.Err == nil && result.units() > 0 {
			delay = time.Second
		}
		if !waitFn(ctx, delay) && ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

func (sum *TranscriptStorageReconcileSummary) add(p TranscriptStorageReconcilePassResult) {
	sum.Passes++
	if p.LeaseHeld {
		sum.LeaseHeldPasses++
	}
	sum.Corrected += p.Corrected
	sum.Confirmed += p.Confirmed
	sum.Raced += p.Raced
	sum.Busy += p.Busy
	sum.Anomaly += p.Anomaly
	sum.Timeout += p.Timeout
	sum.Error += p.Error
	sum.Skipped += p.Skipped
	sum.Entries += p.Entries
	sum.MaxUnitElapsed = max(sum.MaxUnitElapsed, p.MaxUnitElapsed)
}

// ReconcileTranscriptStorageUsageUntilDone never waits Interval between passes.
// Deferred accounts are deliberately not a barrier to completion.
func (s *Store) ReconcileTranscriptStorageUsageUntilDone(ctx context.Context, cfg TranscriptStorageReconcileConfig) (TranscriptStorageReconcileSummary, error) {
	runner := newTranscriptStorageReconcileRunner()
	return reconcileTranscriptStorageUsageUntilDone(ctx, cfg, func(ctx context.Context) TranscriptStorageReconcilePassResult {
		return s.reconcileTranscriptStorageUsagePass(ctx, cfg, runner, nil)
	}, s.TranscriptStorageReconcileStatus, waitForMessageRateBucketCleanupInterval)
}

func reconcileTranscriptStorageUsageUntilDone(ctx context.Context, cfg TranscriptStorageReconcileConfig, passFn func(context.Context) TranscriptStorageReconcilePassResult, statusFn func(context.Context) (TranscriptStorageReconcileStatus, error), waitFn func(context.Context, time.Duration) bool) (summary TranscriptStorageReconcileSummary, err error) {
	start := time.Now()
	defer func() { summary.Elapsed = time.Since(start) }()
	if err = cfg.Validate(); err != nil {
		return
	}
	for {
		if ctx.Err() != nil {
			statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			status, statusErr := statusFn(statusCtx)
			cancel()
			if statusErr == nil {
				summary.UnverifiedEligible = status.UnverifiedEligible
				summary.UnverifiedDeferred = status.UnverifiedDeferred
			}
			return summary, fmt.Errorf("transcript storage reconcile stopped with %d eligible units remaining: %w", summary.UnverifiedEligible, ctx.Err())
		}
		pass := passFn(ctx)
		summary.add(pass)
		status, statusErr := statusFn(ctx)
		if statusErr == nil {
			summary.UnverifiedEligible = status.UnverifiedEligible
			summary.UnverifiedDeferred = status.UnverifiedDeferred
		}
		if ctx.Err() != nil {
			continue
		}
		if pass.Anomaly > 0 {
			return summary, errors.New("transcript storage reconcile anomaly")
		}
		if pass.terminal {
			return summary, errors.New("transcript storage reconcile unit failed three consecutive attempts")
		}
		if statusErr == nil && status.UnverifiedEligible == 0 {
			return summary, nil
		}
		delay := time.Duration(0)
		switch {
		case pass.LeaseHeld || pass.Err != nil || statusErr != nil:
			delay = time.Second
		case pass.units() == 0 && !pass.nextDue.IsZero():
			delay = max(time.Until(pass.nextDue), 0)
		}
		if delay > 0 {
			waitFn(ctx, delay)
		}
	}
}

func transcriptStorageReconcilePacing(query time.Duration, duty float64) time.Duration {
	return time.Duration(float64(query) * (1 - duty) / duty)
}

// Splitting sleeps keeps each statement below its own timeout, while remaining
// active in PostgreSQL so socket checks and the short idle timeout still work.
func transcriptStorageReconcilePace(ctx context.Context, query time.Duration, duty float64, statementTimeout time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) error {
	remaining := transcriptStorageReconcilePacing(query, duty)
	sleepCap := min(10*time.Second, statementTimeout/2)
	if deadline, ok := ctx.Deadline(); ok && remaining > 0 {
		// Estimate the next statement's time from the statement just measured.
		// Reserving its entire timeout cap would make every positive sleep
		// impossible when UnitTimeout is at most that same cap.
		budget := deadline.Sub(now()) - query - time.Second
		if budget <= 0 {
			return context.DeadlineExceeded
		}
		remaining = min(remaining, budget)
	}
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := min(remaining, sleepCap)
		if deadline, ok := ctx.Deadline(); ok && !now().Add(part).Before(deadline.Add(-2*time.Second)) {
			return context.DeadlineExceeded
		}
		if err := sleep(ctx, part); err != nil {
			return err
		}
		remaining -= part
	}
	return ctx.Err()
}
