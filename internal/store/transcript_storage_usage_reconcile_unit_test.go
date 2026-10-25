package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type transcriptStorageReconcileSettingsConn struct {
	t           *testing.T
	statements  []string
	beginErr    error
	probeErr    error
	rollbackErr error
	settingsErr error
	cancelProbe context.CancelFunc
}

func (c *transcriptStorageReconcileSettingsConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.t.Helper()
	if len(args) != 0 {
		c.t.Fatalf("unexpected argument count %d", len(args))
	}
	c.statements = append(c.statements, sql)
	var err error
	switch sql {
	case "BEGIN":
		err = c.beginErr
	case "SET LOCAL client_connection_check_interval = '2s'":
		if c.cancelProbe != nil {
			c.cancelProbe()
		}
		err = c.probeErr
	case "ROLLBACK":
		if ctx.Err() != nil {
			c.t.Fatalf("rollback context already cancelled: %v", ctx.Err())
		}
		if _, ok := ctx.Deadline(); !ok {
			c.t.Fatal("rollback context has no deadline")
		}
		err = c.rollbackErr
	default:
		err = c.settingsErr
	}
	return pgconn.CommandTag{}, err
}

func TestTranscriptStorageUsageReconcileConnectionSettings(t *testing.T) {
	const supportedSQL = `SET application_name = 'witself-transcript-storage-reconcile';
 SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 6;
 SET client_connection_check_interval = '2s'; SET lock_timeout = '500ms'`
	const unsupportedSQL = `SET application_name = 'witself-transcript-storage-reconcile';
 SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 6; SET lock_timeout = '500ms'`
	unsupportedErr := &pgconn.PgError{Code: "22023"}
	otherPgErr := &pgconn.PgError{Code: "57P01"}
	probeErr := errors.New("synthetic probe failure")
	beginErr := errors.New("synthetic begin failure")
	rollbackErr := errors.New("synthetic rollback failure")
	settingsErr := errors.New("synthetic settings failure")
	for _, tc := range []struct {
		name        string
		beginErr    error
		probeErr    error
		rollbackErr error
		settingsErr error
		wantErr     error
		wantSQL     string
		cache       bool
	}{
		{name: "supported", wantSQL: supportedSQL, cache: true},
		{name: "unsupported", probeErr: unsupportedErr, wantSQL: unsupportedSQL, cache: true},
		{name: "wrapped unsupported", probeErr: fmt.Errorf("wrapped: %w", unsupportedErr), wantSQL: unsupportedSQL, cache: true},
		{name: "other PostgreSQL error", probeErr: otherPgErr, wantErr: otherPgErr},
		{name: "driver error", probeErr: probeErr, wantErr: probeErr},
		{name: "context cancelled", probeErr: context.Canceled, wantErr: context.Canceled},
		{name: "context deadline", probeErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "begin failure", beginErr: beginErr, wantErr: beginErr},
		{name: "begin invalid parameter", beginErr: unsupportedErr, wantErr: unsupportedErr},
		{name: "rollback failure after success", rollbackErr: rollbackErr, wantErr: rollbackErr},
		{name: "rollback invalid parameter", rollbackErr: unsupportedErr, wantErr: unsupportedErr},
		{name: "rollback failure after rejection", probeErr: unsupportedErr, rollbackErr: rollbackErr, wantErr: rollbackErr},
		{name: "rollback failure after driver error", probeErr: probeErr, rollbackErr: rollbackErr, wantErr: rollbackErr},
		{name: "rollback failure after begin error", beginErr: beginErr, rollbackErr: rollbackErr, wantErr: rollbackErr},
		{name: "supported settings failure", settingsErr: settingsErr, wantErr: settingsErr, wantSQL: supportedSQL, cache: true},
		{name: "unsupported settings failure", probeErr: unsupportedErr, settingsErr: settingsErr, wantErr: settingsErr, wantSQL: unsupportedSQL, cache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &Store{}
			first := &transcriptStorageReconcileSettingsConn{
				t: t, beginErr: tc.beginErr, probeErr: tc.probeErr,
				rollbackErr: tc.rollbackErr, settingsErr: tc.settingsErr,
			}
			err := st.configureTranscriptStorageReconcileConnection(t.Context(), first)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("configure error = %v, want %v", err, tc.wantErr)
			}
			if tc.rollbackErr != nil {
				for _, cause := range []error{tc.beginErr, tc.probeErr} {
					if cause != nil && !errors.Is(err, cause) {
						t.Fatalf("cleanup failure lost original error: %v", err)
					}
				}
			}
			wantStatements := []string{"BEGIN"}
			if tc.beginErr == nil {
				wantStatements = append(wantStatements, "SET LOCAL client_connection_check_interval = '2s'")
			}
			wantStatements = append(wantStatements, "ROLLBACK")
			if tc.wantSQL != "" {
				wantStatements = append(wantStatements, tc.wantSQL)
			}
			if !slices.Equal(first.statements, wantStatements) {
				t.Fatalf("statements = %q, want %q", first.statements, wantStatements)
			}
			if st.clientConnectionCheckProbed != tc.cache {
				t.Fatalf("cached=%t, want %t", st.clientConnectionCheckProbed, tc.cache)
			}
			// A fresh lease retries transient failures, but reuses either definitive
			// answer even when applying the session settings failed afterward.
			wantSQL := tc.wantSQL
			if !tc.cache {
				wantSQL = supportedSQL
			}
			for i := range 2 {
				next := &transcriptStorageReconcileSettingsConn{t: t}
				if err := st.configureTranscriptStorageReconcileConnection(t.Context(), next); err != nil {
					t.Fatal(err)
				}
				want := []string{wantSQL}
				if !tc.cache && i == 0 {
					want = append([]string{"BEGIN", "SET LOCAL client_connection_check_interval = '2s'", "ROLLBACK"}, want...)
				}
				if !slices.Equal(next.statements, want) {
					t.Fatalf("retry %d statements = %q, want %q", i, next.statements, want)
				}
			}
		})
	}
}

func TestTranscriptStorageUsageReconcileConnectionProbeCancellationCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn := &transcriptStorageReconcileSettingsConn{t: t, cancelProbe: cancel, probeErr: context.Canceled}
	st := &Store{}
	if err := st.configureTranscriptStorageReconcileConnection(ctx, conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("configure error = %v, want cancellation", err)
	}
	want := []string{"BEGIN", "SET LOCAL client_connection_check_interval = '2s'", "ROLLBACK"}
	if !slices.Equal(conn.statements, want) || st.clientConnectionCheckProbed {
		t.Fatalf("cancelled probe statements=%q cached=%t", conn.statements, st.clientConnectionCheckProbed)
	}
}

func TestTranscriptStorageUsageReconcileConfig(t *testing.T) {
	defaults := DefaultTranscriptStorageReconcileConfig()
	if defaults != (TranscriptStorageReconcileConfig{5 * time.Minute, .5, 1000, 240 * time.Second}) {
		t.Fatalf("defaults = %+v", defaults)
	}
	for _, tc := range []struct {
		name  string
		edit  func(*TranscriptStorageReconcileConfig)
		valid bool
	}{
		{"defaults", func(*TranscriptStorageReconcileConfig) {}, true},
		{"minimums", func(c *TranscriptStorageReconcileConfig) {
			*c = TranscriptStorageReconcileConfig{10 * time.Second, .05, 1, 10 * time.Second}
		}, true},
		{"maximums", func(c *TranscriptStorageReconcileConfig) {
			*c = TranscriptStorageReconcileConfig{time.Hour, 1, 10000, 280 * time.Second}
		}, true},
		{"interval below", func(c *TranscriptStorageReconcileConfig) { c.Interval = 10*time.Second - 1 }, false},
		{"interval above", func(c *TranscriptStorageReconcileConfig) { c.Interval = time.Hour + 1 }, false},
		{"duty below", func(c *TranscriptStorageReconcileConfig) { c.DutyCycle = .049 }, false},
		{"duty above", func(c *TranscriptStorageReconcileConfig) { c.DutyCycle = 1.001 }, false},
		{"duty nan", func(c *TranscriptStorageReconcileConfig) { c.DutyCycle = math.NaN() }, false},
		{"duty positive infinity", func(c *TranscriptStorageReconcileConfig) { c.DutyCycle = math.Inf(1) }, false},
		{"duty negative infinity", func(c *TranscriptStorageReconcileConfig) { c.DutyCycle = math.Inf(-1) }, false},
		{"page below", func(c *TranscriptStorageReconcileConfig) { c.PageEntries = 0 }, false},
		{"page above", func(c *TranscriptStorageReconcileConfig) { c.PageEntries = 10001 }, false},
		{"timeout below", func(c *TranscriptStorageReconcileConfig) { c.UnitTimeout = 10*time.Second - 1 }, false},
		{"timeout above", func(c *TranscriptStorageReconcileConfig) { c.UnitTimeout = 280*time.Second + 1 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults
			tc.edit(&cfg)
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tc.valid)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcileWorkerLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := DefaultTranscriptStorageReconcileConfig()
	failure := errors.New("synthetic pass failure")
	passes := []TranscriptStorageReconcilePassResult{{}, {LeaseHeld: true}, {Corrected: 1}, {Err: failure}, {Timeout: 1}, {}}
	wantWait := []time.Duration{cfg.Interval, cfg.Interval, time.Second, cfg.Interval, time.Second, cfg.Interval}
	var calls, callbacks, waits int
	err := runTranscriptStorageReconcileWorker(ctx, cfg, func(context.Context) TranscriptStorageReconcilePassResult {
		if calls >= len(passes) {
			t.Fatal("worker exceeded fixture")
		}
		result := passes[calls]
		calls++
		return result
	}, func(waitCtx context.Context, d time.Duration) bool {
		if callbacks != calls {
			t.Fatal("wait started before result callback")
		}
		if d != wantWait[waits] {
			t.Fatalf("wait %d = %v, want %v", waits, d, wantWait[waits])
		}
		waits++
		if waits == len(passes) {
			cancel()
		}
		// A spurious interrupted wait cannot end a healthy worker.
		if waits == 1 {
			return false
		}
		return waitCtx.Err() == nil
	}, func(result TranscriptStorageReconcilePassResult) {
		if !reflect.DeepEqual(result, passes[callbacks]) {
			t.Fatalf("callback %d = %+v", callbacks, result)
		}
		callbacks++
	})
	if err != nil || calls != len(passes) || callbacks != len(passes) || waits != len(passes) {
		t.Fatalf("loop = %v; calls/callbacks/waits = %d/%d/%d", err, calls, callbacks, waits)
	}
}

func TestTranscriptStorageUsageReconcileWorkerInvalidAndCancelled(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cfg := DefaultTranscriptStorageReconcileConfig()
		if invalid {
			cfg.PageEntries = 0
		}
		err := runTranscriptStorageReconcileWorker(ctx, cfg, func(context.Context) TranscriptStorageReconcilePassResult {
			t.Fatal("pass called before config/cancellation check")
			return TranscriptStorageReconcilePassResult{}
		}, nil, nil)
		if (err != nil) != invalid {
			t.Fatalf("invalid=%v returned %v", invalid, err)
		}
	}
}

func TestTranscriptStorageUsageReconcilePacing(t *testing.T) {
	for _, tc := range []struct {
		query time.Duration
		duty  float64
		want  time.Duration
	}{
		{9 * time.Second, .05, 171 * time.Second}, {9 * time.Second, .5, 9 * time.Second}, {9 * time.Second, 1, 0}, {0, .05, 0},
	} {
		if got := transcriptStorageReconcilePacing(tc.query, tc.duty); got != tc.want {
			t.Fatalf("pacing(%v,%v)=%v, want %v", tc.query, tc.duty, got, tc.want)
		}
	}
	for _, statementTimeout := range []time.Duration{60 * time.Second, 6 * time.Second} {
		var total time.Duration
		var calls int
		err := transcriptStorageReconcilePace(t.Context(), 9*time.Second, .05, statementTimeout, time.Now, func(_ context.Context, d time.Duration) error {
			if d <= 0 || d > min(10*time.Second, statementTimeout/2) {
				t.Fatalf("sleep exceeds statement cap: %v", d)
			}
			total += d
			calls++
			return nil
		})
		if err != nil || total != 171*time.Second || calls < 2 {
			t.Fatalf("split pacing = %v, %v over %d calls", err, total, calls)
		}
	}
}

func TestTranscriptStorageUsageReconcilePacingDeadline(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(t.Context(), now.Add(100*time.Second))
	defer cancel()
	var total time.Duration
	err := transcriptStorageReconcilePace(ctx, 9*time.Second, .05, 60*time.Second, func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
		total += d
		now = now.Add(d)
		return nil
	})
	if err != nil || total != 90*time.Second {
		t.Fatalf("deadline clamp = %v, %v; want 90s", err, total)
	}

	now = time.Now()
	ctx, cancel = context.WithDeadline(t.Context(), now.Add(100*time.Second))
	defer cancel()
	clockCalls, sleeps := 0, 0
	err = transcriptStorageReconcilePace(ctx, 9*time.Second, .05, 60*time.Second, func() time.Time {
		clockCalls++
		if clockCalls > 1 {
			return now.Add(95 * time.Second)
		}
		return now
	}, func(context.Context, time.Duration) error { sleeps++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || sleeps != 0 {
		t.Fatalf("late sleep = %v, issued %d statements", err, sleeps)
	}
}

func TestTranscriptStorageUsageReconcilePacingErrors(t *testing.T) {
	statementErr := &pgconn.PgError{Code: "57014"}
	for _, cause := range []error{statementErr, context.DeadlineExceeded, errors.New("synthetic query failure")} {
		err := transcriptStorageReconcilePace(t.Context(), time.Second, .5, 60*time.Second, time.Now, func(context.Context, time.Duration) error { return cause })
		if !errors.Is(err, cause) {
			t.Fatalf("sleep error = %v, want cause", err)
		}
		want := "error"
		if cause == statementErr || errors.Is(cause, context.DeadlineExceeded) {
			want = "timeout"
		}
		if got := transcriptStorageReconcileErrorResult(err); got != want {
			t.Fatalf("error result = %s, want %s", got, want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := transcriptStorageReconcilePace(ctx, time.Second, .5, time.Minute, time.Now, func(context.Context, time.Duration) error {
		t.Fatal("cancelled pacing issued a statement")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pacing = %v", err)
	}
}

func TestTranscriptStorageUsageReconcileBackoff(t *testing.T) {
	unit := transcriptStorageReconcileUnit{"fixture", 2}
	now := time.Now()
	runner := newTranscriptStorageReconcileRunner()
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if runner.record(unit, "busy", now) {
			t.Fatalf("busy attempt %d ended runner", i)
		}
		if got := runner.backoff[unit].until.Sub(now); got != want {
			t.Fatalf("busy backoff %d = %v, want %v", i, got, want)
		}
	}
	if runner.record(unit, "raced", now) || runner.backoff[unit].until.Sub(now) != 30*time.Second {
		t.Fatal("raced must only back off 30s")
	}
	if newTranscriptStorageReconcileRunner().stuck() != 0 || len(newTranscriptStorageReconcileRunner().backoff) != 0 {
		t.Fatal("backoff leaked between runners")
	}
	for _, outcome := range []string{"corrected", "confirmed", "skipped"} {
		runner.record(unit, "timeout", now)
		runner.record(unit, outcome, now)
		if len(runner.backoff) != 0 {
			t.Fatalf("%s did not clear backoff", outcome)
		}
	}
}

func TestTranscriptStorageUsageReconcileRacedResetsBackoff(t *testing.T) {
	unit := transcriptStorageReconcileUnit{"fixture", 2}
	now := time.Now()
	for _, prior := range [][]string{nil, {"timeout", "timeout", "timeout"}} {
		runner := newTranscriptStorageReconcileRunner()
		for _, outcome := range prior {
			runner.record(unit, outcome, now)
		}
		for range 2 {
			if runner.record(unit, "raced", now) {
				t.Fatal("race ended runner")
			}
			b := runner.backoff[unit]
			if b.until.Sub(now) != 30*time.Second || b.failures != 0 || b.timeouts != 0 || b.hardFailures != 0 || runner.stuck() != 0 {
				t.Fatalf("race did not reset failure history: %+v", b)
			}
		}
		if runner.record(unit, "busy", now) || runner.backoff[unit].until.Sub(now) != time.Minute || runner.backoff[unit].failures != 1 {
			t.Fatalf("busy after raced, raced did not restart at one minute: %+v", runner.backoff[unit])
		}
	}
}

func TestTranscriptStorageUsageReconcileStuckAndHardFailures(t *testing.T) {
	unit := transcriptStorageReconcileUnit{"fixture", 1}
	now := time.Now()
	runner := newTranscriptStorageReconcileRunner()
	for i := 1; i <= 3; i++ {
		terminal := runner.record(unit, "timeout", now)
		if terminal != (i == 3) || (runner.stuck() == 1) != (i == 3) {
			t.Fatalf("timeout %d: terminal=%v stuck=%d", i, terminal, runner.stuck())
		}
	}
	runner.record(unit, "confirmed", now)
	if runner.stuck() != 0 {
		t.Fatal("success did not clear stuck count")
	}
	for _, outcome := range []string{"timeout", "timeout", "busy", "timeout", "error"} {
		if runner.record(unit, outcome, now) {
			t.Fatalf("non-consecutive failure ended on %s", outcome)
		}
	}
	if runner.stuck() != 0 {
		t.Fatal("non-timeout did not reset consecutive timeout count")
	}
	if !runner.record(unit, "error", now) {
		t.Fatal("three consecutive mixed errors/timeouts did not end runner")
	}
	runner.record(unit, "confirmed", now)
	if !runner.record(unit, "anomaly", now) {
		t.Fatal("first anomaly did not end runner")
	}
}

func TestTranscriptStorageUsageReconcileBackoffOverride(t *testing.T) {
	prior := transcriptStorageReconcileTestHooks
	t.Cleanup(func() { transcriptStorageReconcileTestHooks = prior })
	fixed := 17 * time.Millisecond
	transcriptStorageReconcileTestHooks = &transcriptStorageReconcileHooks{backoff: &fixed}
	now := time.Now()
	runner := newTranscriptStorageReconcileRunner()
	for _, outcome := range []string{"busy", "raced", "timeout", "error"} {
		unit := transcriptStorageReconcileUnit{outcome, 1}
		runner.record(unit, outcome, now)
		if got := runner.backoff[unit].until.Sub(now); got != fixed {
			t.Fatalf("override for %s = %v", outcome, got)
		}
	}
}

type transcriptStorageFakeReconcileSession struct {
	pending            map[transcriptStorageReconcileUnit]bool
	outcomes           map[transcriptStorageReconcileUnit]string
	measured, picks    []transcriptStorageReconcileUnit
	events             []string
	invalidated        bool
	statusErr, pickErr error
	measureErr         error
	onMeasure          func()
	onContext          func(string, context.Context)
	onPick             func()
}

func (f *transcriptStorageFakeReconcileSession) status(ctx context.Context) (TranscriptStorageReconcileStatus, error) {
	if f.onContext != nil {
		f.onContext("status", ctx)
	}
	f.events = append(f.events, "status")
	return TranscriptStorageReconcileStatus{UnverifiedEligible: int64(len(f.pending)), UnverifiedDeferred: 3}, f.statusErr
}
func (f *transcriptStorageFakeReconcileSession) pick(ctx context.Context, cursor transcriptStorageReconcileUnit) ([]transcriptStorageReconcileUnit, error) {
	if f.onContext != nil {
		f.onContext("pick", ctx)
	}
	f.events = append(f.events, "pick")
	f.picks = append(f.picks, cursor)
	if f.onPick != nil {
		f.onPick()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var units []transcriptStorageReconcileUnit
	for unit := range f.pending {
		if unit.accountID > cursor.accountID || (unit.accountID == cursor.accountID && unit.shard > cursor.shard) {
			units = append(units, unit)
		}
	}
	slices.SortFunc(units, func(a, b transcriptStorageReconcileUnit) int {
		if a.accountID != b.accountID {
			return strings.Compare(a.accountID, b.accountID)
		}
		return int(a.shard - b.shard)
	})
	return units, f.pickErr
}
func (f *transcriptStorageFakeReconcileSession) measure(ctx context.Context, _ TranscriptStorageReconcileConfig, unit transcriptStorageReconcileUnit) (transcriptStorageReconcileMeasurement, string, error) {
	if f.onContext != nil {
		f.onContext("measure", ctx)
	}
	f.events = append(f.events, "measure")
	f.measured = append(f.measured, unit)
	if f.onMeasure != nil {
		f.onMeasure()
	}
	measurement := transcriptStorageReconcileMeasurement{entries: 7}
	if _, ok := ctx.Deadline(); !ok {
		return measurement, "", errors.New("unit deadline missing")
	}
	if err := ctx.Err(); err != nil {
		return measurement, "", err
	}
	if f.measureErr != nil {
		return measurement, "", f.measureErr
	}
	if f.outcomes[unit] == "timeout" {
		return measurement, "", &pgconn.PgError{Code: "57014"}
	}
	return measurement, "", nil
}
func (f *transcriptStorageFakeReconcileSession) apply(ctx context.Context, unit transcriptStorageReconcileUnit, _ transcriptStorageReconcileMeasurement) (string, error) {
	if f.onContext != nil {
		f.onContext("apply", ctx)
	}
	f.events = append(f.events, "apply")
	outcome := f.outcomes[unit]
	if outcome == "" {
		outcome = "confirmed"
	}
	if outcome == "confirmed" || outcome == "corrected" {
		delete(f.pending, unit)
	}
	return outcome, nil
}
func (f *transcriptStorageFakeReconcileSession) invalidate()   { f.invalidated = true }
func (f *transcriptStorageFakeReconcileSession) invalid() bool { return f.invalidated }

func TestTranscriptStorageUsageReconcilePassBackoffFairness(t *testing.T) {
	for _, outcome := range []string{"busy", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			units := []transcriptStorageReconcileUnit{{"a", 0}, {"b", 0}, {"c", 0}}
			pending := map[transcriptStorageReconcileUnit]bool{units[0]: true, units[1]: true, units[2]: true}
			runner := newTranscriptStorageReconcileRunner()
			now := time.Now()
			var attempted []transcriptStorageReconcileUnit
			for range 3 {
				fake := &transcriptStorageFakeReconcileSession{pending: pending, outcomes: map[transcriptStorageReconcileUnit]string{units[0]: outcome}}
				result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, func() time.Time { return now }, nil)
				if result.Err != nil {
					t.Fatal(result.Err)
				}
				attempted = append(attempted, fake.measured...)
			}
			if !reflect.DeepEqual(attempted, units) || len(pending) != 1 || !pending[units[0]] {
				t.Fatalf("failed unit starved others or ignored backoff: attempted=%v pending=%v", attempted, pending)
			}
			now = now.Add(time.Minute)
			fake := &transcriptStorageFakeReconcileSession{pending: pending, outcomes: map[transcriptStorageReconcileUnit]string{units[0]: outcome}}
			runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, func() time.Time { return now }, nil)
			if !reflect.DeepEqual(fake.measured, units[:1]) {
				t.Fatalf("expired backoff did not retry failed unit: %v", fake.measured)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassCursorAndStatus(t *testing.T) {
	units := []transcriptStorageReconcileUnit{{"a", 0}, {"b", 0}, {"c", 0}}
	fake := &transcriptStorageFakeReconcileSession{pending: map[transcriptStorageReconcileUnit]bool{units[0]: true, units[1]: true, units[2]: true}}
	runner := newTranscriptStorageReconcileRunner()
	runner.cursor = units[1]
	var statuses []TranscriptStorageReconcileStatus
	result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, time.Now, func(s TranscriptStorageReconcileStatus) {
		if len(statuses) == 0 && len(fake.measured) != 0 {
			t.Fatal("first status published after first unit")
		}
		statuses = append(statuses, s)
	})
	if !reflect.DeepEqual(fake.measured, []transcriptStorageReconcileUnit{units[2], units[0], units[1]}) {
		t.Fatalf("cursor order = %v", fake.measured)
	}
	var wraps int
	for _, cursor := range fake.picks {
		if cursor.shard == -1 {
			wraps++
		}
	}
	if wraps != 1 {
		t.Fatalf("cursor wrapped %d times", wraps)
	}
	if len(statuses) != 2 || statuses[0].UnverifiedEligible != 3 || statuses[1].UnverifiedEligible != 0 || result.UnverifiedEligible != 0 || result.UnverifiedDeferred != 3 || result.Confirmed != 3 || result.Entries != 21 {
		t.Fatalf("status/results = %+v / %+v", statuses, result)
	}
}

func TestTranscriptStorageUsageReconcilePassLeaseHeldAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name               string
		held               bool
		statusErr, pickErr error
		published, picks   int
	}{
		{"lease held elsewhere", false, nil, nil, 1, 0},
		{"status failed", true, errors.New("synthetic status error"), nil, 0, 0},
		{"pick failed", true, nil, errors.New("synthetic pick error"), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &transcriptStorageFakeReconcileSession{pending: map[transcriptStorageReconcileUnit]bool{{"a", 0}: true}, statusErr: tc.statusErr, pickErr: tc.pickErr}
			var published int
			result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), newTranscriptStorageReconcileRunner(), fake, tc.held, time.Now, func(s TranscriptStorageReconcileStatus) {
				published++
				if s.UnverifiedEligible != 1 || s.UnverifiedDeferred != 3 {
					t.Fatalf("incorrect status %+v", s)
				}
			})
			wantError := tc.statusErr != nil || tc.pickErr != nil
			if published != tc.published || len(fake.picks) != tc.picks || (result.Err != nil) != wantError || result.LeaseHeld == tc.held || fake.invalidated != wantError {
				t.Fatalf("published=%d picks=%d invalid=%v result=%+v", published, len(fake.picks), fake.invalidated, result)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassInterval(t *testing.T) {
	cfg := DefaultTranscriptStorageReconcileConfig()
	cfg.Interval = 10 * time.Second
	cfg.UnitTimeout = 280 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var passDeadline, unitDeadline time.Time
	seen := map[string]int{}
	fake := &transcriptStorageFakeReconcileSession{
		pending: map[transcriptStorageReconcileUnit]bool{{"a", 0}: true},
		onContext: func(operation string, ctx context.Context) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("%s lacks a deadline", operation)
			}
			switch operation {
			case "status", "pick":
				if operation == "status" && seen["status"] != 0 {
					if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second || deadline.Equal(passDeadline) {
						t.Fatalf("final status did not retain its independent five-second bound: %s", remaining)
					}
					break
				}
				if time.Until(deadline) > cfg.Interval {
					t.Fatalf("%s is not bounded by pass interval", operation)
				}
				if passDeadline.IsZero() {
					passDeadline = deadline
				}
				if !deadline.Equal(passDeadline) {
					t.Fatalf("%s did not inherit the pass deadline", operation)
				}
			case "measure", "apply":
				if remaining := time.Until(deadline); remaining > cfg.UnitTimeout || remaining <= cfg.Interval {
					t.Fatalf("%s did not retain its independent unit timeout: %s", operation, remaining)
				}
				if unitDeadline.IsZero() {
					unitDeadline = deadline
				}
				if !deadline.Equal(unitDeadline) {
					t.Fatalf("%s did not inherit the unit deadline", operation)
				}
			}
			seen[operation]++
		},
	}
	result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), cfg, newTranscriptStorageReconcileRunner(), fake, true, time.Now, nil)
	if result.Err != nil || result.Confirmed != 1 || seen["status"] != 2 || seen["pick"] == 0 || seen["measure"] != 1 || seen["apply"] != 1 {
		t.Fatalf("bounded pass operations=%v result=%+v", seen, result)
	}
}

func TestTranscriptStorageUsageReconcilePassPickIntervalStop(t *testing.T) {
	for _, completed := range []int64{0, 1} {
		t.Run(fmt.Sprintf("completed_%d", completed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cfg := DefaultTranscriptStorageReconcileConfig()
				cfg.Interval = 10 * time.Second
				runner := newTranscriptStorageReconcileRunner()
				picks := 0
				fake := &transcriptStorageFakeReconcileSession{
					pending: map[transcriptStorageReconcileUnit]bool{{"a", 0}: true},
					onPick: func() {
						picks++
						if picks == int(completed)+1 {
							time.Sleep(11 * time.Second)
						}
					},
				}
				err := runTranscriptStorageReconcileWorker(ctx, cfg, func(ctx context.Context) TranscriptStorageReconcilePassResult {
					result := runTranscriptStorageReconcilePass(ctx, ctx, cfg, runner, fake, true, time.Now, nil)
					if result.Err != nil || result.Timeout != 0 || result.Error != 0 || result.Anomaly != 0 || !fake.invalidated {
						t.Fatalf("Interval-cut-off pick recorded a job failure or kept its connection: invalidated=%v result=%+v", fake.invalidated, result)
					}
					if result.Confirmed != completed || int64(len(fake.measured)) != completed || picks != int(completed)+1 || result.Elapsed != 11*time.Second {
						t.Fatalf("Interval-cut-off pick admitted work: measured=%v picks=%d result=%+v", fake.measured, picks, result)
					}
					return result
				}, func(_ context.Context, delay time.Duration) bool {
					want := cfg.Interval
					if completed > 0 {
						want = time.Second
					}
					if delay != want {
						t.Fatalf("Interval-cut-off pick delay = %s, want %s", delay, want)
					}
					cancel()
					return false
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestTranscriptStorageUsageReconcilePassUnitCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var measureCtx context.Context
	fake := &transcriptStorageFakeReconcileSession{
		pending: map[transcriptStorageReconcileUnit]bool{{"a", 0}: true, {"b", 0}: true},
		onContext: func(operation string, ctx context.Context) {
			if operation == "measure" {
				measureCtx = ctx
			}
		},
		onMeasure: func() {
			cancel()
			if measureCtx == nil || measureCtx.Err() == nil {
				t.Error("caller cancellation did not immediately cancel the in-flight measure context")
			}
		},
	}
	result := runTranscriptStorageReconcilePass(ctx, ctx, DefaultTranscriptStorageReconcileConfig(), newTranscriptStorageReconcileRunner(), fake, true, time.Now, nil)
	if result.Error != 1 || result.Timeout != 0 || result.units() != 1 || !fake.invalidated || len(fake.measured) != 1 || slices.Contains(fake.events, "apply") {
		t.Fatalf("caller cancellation did not end the unit and pass: invalidated=%v events=%v result=%+v", fake.invalidated, fake.events, result)
	}
}

func TestTranscriptStorageUsageReconcilePassUnitCrossesInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := DefaultTranscriptStorageReconcileConfig()
		cfg.Interval = 10 * time.Second
		cfg.UnitTimeout = 20 * time.Second
		start := time.Now()
		unit := transcriptStorageReconcileUnit{"a", 0}
		runner := newTranscriptStorageReconcileRunner()
		statuses := 0
		fake := &transcriptStorageFakeReconcileSession{
			pending: map[transcriptStorageReconcileUnit]bool{unit: true, {"b", 0}: true},
			onContext: func(operation string, ctx context.Context) {
				if operation == "status" {
					statuses++
					if statuses == 2 {
						deadline, ok := ctx.Deadline()
						if !ok || ctx.Err() != nil || time.Until(deadline) != 5*time.Second {
							t.Fatal("final status inherited the expired pass deadline")
						}
					}
				}
			},
			onPick: func() { time.Sleep(9 * time.Second) },
			onMeasure: func() {
				if elapsed := time.Since(start); elapsed != cfg.Interval-time.Second {
					t.Fatalf("unit started at %s, want one second before Interval", elapsed)
				}
				time.Sleep(3 * time.Second)
			},
		}
		result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), cfg, runner, fake, true, time.Now, nil)
		if result.Confirmed != 1 || result.Timeout != 0 || result.Error != 0 || result.Err != nil || result.terminal || result.Stuck != 0 || runner.stuck() != 0 || len(runner.backoff) != 0 || fake.invalid() {
			t.Fatalf("unit crossing Interval became a failure: result=%+v backoff=%v", result, runner.backoff)
		}
		if result.Elapsed != 12*time.Second || !reflect.DeepEqual(fake.measured, []transcriptStorageReconcileUnit{unit}) || len(fake.picks) != 1 || statuses != 2 {
			t.Fatalf("pass admission or final status incorrect: elapsed=%s measured=%v picks=%d statuses=%d", result.Elapsed, fake.measured, len(fake.picks), statuses)
		}
	})
}

func TestTranscriptStorageUsageReconcilePassPrunesFullCycle(t *testing.T) {
	for _, keepBackoffUnit := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep_backoff_unit_%t", keepBackoffUnit), func(t *testing.T) {
			now := time.Now()
			stale, retained := transcriptStorageReconcileUnit{"a", 0}, transcriptStorageReconcileUnit{"b", 0}
			runner := newTranscriptStorageReconcileRunner()
			for range 3 {
				runner.record(stale, "timeout", now)
			}
			if runner.stuck() != 1 {
				t.Fatal("fixture did not mark the absent unit stuck")
			}
			fake := &transcriptStorageFakeReconcileSession{pending: map[transcriptStorageReconcileUnit]bool{}}
			if keepBackoffUnit {
				fake.pending[retained] = true
				runner.record(retained, "busy", now)
			}
			var finalStuck int64 = -1
			result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, func() time.Time { return now }, func(status TranscriptStorageReconcileStatus) { finalStuck = status.Stuck })
			if _, ok := runner.backoff[stale]; ok || runner.stuck() != 0 || result.Stuck != 0 || finalStuck != 0 || result.Err != nil {
				t.Fatalf("full cycle retained absent stuck unit: result=%+v backoff=%v final_stuck=%d", result, runner.backoff, finalStuck)
			}
			if _, ok := runner.backoff[retained]; ok != keepBackoffUnit || len(fake.measured) != 0 {
				t.Fatalf("full cycle pruned or attempted a seen backoff unit: backoff=%v measured=%v", runner.backoff, fake.measured)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassKeepsIncompleteCycle(t *testing.T) {
	for _, stop := range []string{"interval", "cancelled", "invalid lease", "terminal outcome", "pick error", "interval at wrapped empty pick", "cancelled at wrapped empty pick"} {
		t.Run(stop, func(t *testing.T) {
			cfg := DefaultTranscriptStorageReconcileConfig()
			now := time.Now()
			stale, active := transcriptStorageReconcileUnit{"a", 0}, transcriptStorageReconcileUnit{"b", 0}
			runner := newTranscriptStorageReconcileRunner()
			for range 3 {
				runner.record(stale, "timeout", now)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fake := &transcriptStorageFakeReconcileSession{pending: map[transcriptStorageReconcileUnit]bool{active: true}}
			switch stop {
			case "interval":
				fake.onPick = func() { now = now.Add(cfg.Interval) }
			case "cancelled":
				fake.onPick = cancel
			case "invalid lease":
				fake.onMeasure = fake.invalidate
			case "terminal outcome":
				fake.outcomes = map[transcriptStorageReconcileUnit]string{active: "anomaly"}
			case "pick error":
				fake.pickErr = errors.New("synthetic pick error")
			case "interval at wrapped empty pick", "cancelled at wrapped empty pick":
				fake.pending = nil
				picks := 0
				fake.onPick = func() {
					picks++
					if picks == 2 {
						if stop == "interval at wrapped empty pick" {
							now = now.Add(cfg.Interval)
						} else {
							cancel()
						}
					}
				}
			}
			result := runTranscriptStorageReconcilePass(ctx, ctx, cfg, runner, fake, true, func() time.Time { return now }, nil)
			if _, ok := runner.backoff[stale]; !ok || runner.stuck() != 1 || result.Stuck != 1 {
				t.Fatalf("incomplete cycle pruned absent stuck unit: result=%+v backoff=%v", result, runner.backoff)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassLostLease(t *testing.T) {
	units := []transcriptStorageReconcileUnit{{"a", 0}, {"b", 0}, {"c", 0}}
	runner := newTranscriptStorageReconcileRunner()
	fake := &transcriptStorageFakeReconcileSession{
		pending:    map[transcriptStorageReconcileUnit]bool{units[0]: true, units[1]: true, units[2]: true},
		measureErr: errors.New("synthetic closed connection"),
	}
	fake.onMeasure = fake.invalidate
	result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, time.Now, nil)
	if result.Err == nil || result.Err.Error() != "transcript storage reconcile lease lost" || result.Error != 1 || result.Timeout != 0 || result.units() != 1 || !reflect.DeepEqual(fake.measured, units[:1]) {
		t.Fatalf("lost lease poisoned remaining candidates: result=%+v measured=%v", result, fake.measured)
	}
	if len(runner.backoff) != 1 || runner.backoff[units[0]].hardFailures != 1 {
		t.Fatalf("lost lease changed untouched candidates' backoff: %v", runner.backoff)
	}
}

func TestTranscriptStorageUsageReconcileUntilDoneTerminal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outcomes   []string
		wantPasses int
		wantError  bool
	}{
		{"anomaly", []string{"anomaly"}, 1, true},
		{"three errors", []string{"error", "error", "error"}, 3, true},
		{"three timeouts", []string{"timeout", "timeout", "timeout"}, 3, true},
		{"raced and busy continue", []string{"raced", "busy", "raced", "busy", "confirmed"}, 5, false},
		{"busy resets errors", []string{"error", "error", "busy", "error", "confirmed"}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newTranscriptStorageReconcileRunner()
			unit := transcriptStorageReconcileUnit{"fixture", 0}
			var calls int
			remaining := int64(1)
			summary, err := reconcileTranscriptStorageUsageUntilDone(t.Context(), DefaultTranscriptStorageReconcileConfig(), func(context.Context) TranscriptStorageReconcilePassResult {
				if calls >= len(tc.outcomes) {
					t.Fatal("UntilDone did not terminate")
				}
				outcome := tc.outcomes[calls]
				calls++
				if outcome == "confirmed" {
					remaining = 0
				}
				result := TranscriptStorageReconcilePassResult{Entries: 7, MaxUnitElapsed: time.Duration(calls) * time.Millisecond, terminal: runner.record(unit, outcome, time.Now())}
				result.add(outcome)
				return result
			}, func(context.Context) (TranscriptStorageReconcileStatus, error) {
				return TranscriptStorageReconcileStatus{UnverifiedEligible: remaining, UnverifiedDeferred: 2}, nil
			}, func(context.Context, time.Duration) bool { t.Fatal("work pass waited before next pass"); return false })
			if (err != nil) != tc.wantError || calls != tc.wantPasses || summary.Passes != calls || summary.Entries != int64(calls*7) || summary.MaxUnitElapsed != time.Duration(calls)*time.Millisecond || summary.UnverifiedEligible != remaining || summary.UnverifiedDeferred != 2 || summary.Elapsed <= 0 {
				t.Fatalf("UntilDone = %+v, %v; calls=%d", summary, err, calls)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcileUntilDoneWaits(t *testing.T) {
	cfg := DefaultTranscriptStorageReconcileConfig()
	earliest := time.Now().Add(7 * time.Second)
	passes := []TranscriptStorageReconcilePassResult{{LeaseHeld: true}, {nextDue: earliest}, {Corrected: 1}}
	calls, waits := 0, 0
	summary, err := reconcileTranscriptStorageUsageUntilDone(t.Context(), cfg, func(context.Context) TranscriptStorageReconcilePassResult {
		if calls >= len(passes) {
			t.Fatal("UntilDone did not stop on exact final status")
		}
		p := passes[calls]
		calls++
		return p
	}, func(context.Context) (TranscriptStorageReconcileStatus, error) {
		if calls == len(passes) {
			return TranscriptStorageReconcileStatus{UnverifiedDeferred: 4}, nil
		}
		return TranscriptStorageReconcileStatus{UnverifiedEligible: 1}, nil
	}, func(_ context.Context, d time.Duration) bool {
		if waits == 0 && d != time.Second {
			t.Fatalf("lease wait = %v", d)
		}
		if waits == 1 && (d <= 0 || d > 7*time.Second) {
			t.Fatalf("earliest-backoff wait = %v", d)
		}
		if waits >= 2 || d == cfg.Interval {
			t.Fatalf("unexpected wait %d: %v", waits, d)
		}
		waits++
		return true
	})
	if err != nil || calls != 3 || waits != 2 || summary.LeaseHeldPasses != 1 || summary.UnverifiedEligible != 0 || summary.UnverifiedDeferred != 4 {
		t.Fatalf("UntilDone waits = %+v, %v, calls=%d waits=%d", summary, err, calls, waits)
	}
}

func TestTranscriptStorageUsageReconcileUntilDoneCancellation(t *testing.T) {
	for _, cancelInPass := range []bool{false, true} {
		t.Run(map[bool]string{false: "during wait", true: "during final pass"}[cancelInPass], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var finalStatus bool
			summary, err := reconcileTranscriptStorageUsageUntilDone(ctx, DefaultTranscriptStorageReconcileConfig(), func(context.Context) TranscriptStorageReconcilePassResult {
				if cancelInPass {
					cancel()
				}
				return TranscriptStorageReconcilePassResult{LeaseHeld: true}
			}, func(statusCtx context.Context) (TranscriptStorageReconcileStatus, error) {
				if ctx.Err() != nil && statusCtx.Err() == nil {
					finalStatus = true
				}
				// Even a zero count observed with a cancelled ctx must not report success.
				if statusCtx.Err() != nil {
					return TranscriptStorageReconcileStatus{}, nil
				}
				return TranscriptStorageReconcileStatus{UnverifiedEligible: 7, UnverifiedDeferred: 3}, nil
			}, func(_ context.Context, d time.Duration) bool {
				if d != time.Second {
					t.Fatalf("wait=%v", d)
				}
				cancel()
				return false
			})
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "7 eligible units remaining") || summary.UnverifiedEligible != 7 || summary.UnverifiedDeferred != 3 || !finalStatus {
				t.Fatalf("cancelled UntilDone = %+v, %v, final status=%v", summary, err, finalStatus)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassTerminalAndTiming(t *testing.T) {
	for _, outcome := range []string{"anomaly", "error"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now()
			unit := transcriptStorageReconcileUnit{"a", 0}
			runner := newTranscriptStorageReconcileRunner()
			if outcome == "error" {
				runner.record(unit, outcome, now.Add(-time.Hour))
				runner.record(unit, outcome, now.Add(-time.Hour))
			}
			fake := &transcriptStorageFakeReconcileSession{
				pending:   map[transcriptStorageReconcileUnit]bool{unit: true, {"b", 0}: true},
				outcomes:  map[transcriptStorageReconcileUnit]string{unit: outcome},
				onPick:    func() { now = now.Add(2 * time.Second) },
				onMeasure: func() { now = now.Add(3 * time.Second) },
			}
			result := runTranscriptStorageReconcilePass(t.Context(), t.Context(), DefaultTranscriptStorageReconcileConfig(), runner, fake, true, func() time.Time { return now }, nil)
			if !result.terminal || len(fake.measured) != 1 || result.MaxUnitElapsed != 5*time.Second || result.UnverifiedEligible != 2 {
				t.Fatalf("terminal result or pick accounting = %+v, measured=%d", result, len(fake.measured))
			}
		})
	}
}

func TestTranscriptStorageUsageReconcileUntilDoneInvalidAndStatusFailure(t *testing.T) {
	cfg := DefaultTranscriptStorageReconcileConfig()
	cfg.PageEntries = 0
	_, err := reconcileTranscriptStorageUsageUntilDone(t.Context(), cfg, func(context.Context) TranscriptStorageReconcilePassResult {
		t.Fatal("invalid config ran a pass")
		return TranscriptStorageReconcilePassResult{}
	}, nil, nil)
	if err == nil {
		t.Fatal("invalid config was accepted")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls, waits := 0, 0
	summary, err := reconcileTranscriptStorageUsageUntilDone(ctx, DefaultTranscriptStorageReconcileConfig(), func(context.Context) TranscriptStorageReconcilePassResult {
		calls++
		if calls > 1 {
			t.Fatal("cancelled status retry ran another pass")
		}
		return TranscriptStorageReconcilePassResult{Confirmed: 1}
	}, func(context.Context) (TranscriptStorageReconcileStatus, error) {
		return TranscriptStorageReconcileStatus{}, errors.New("synthetic status error")
	}, func(waitCtx context.Context, d time.Duration) bool {
		if waitCtx != ctx || d != time.Second {
			t.Fatalf("status retry wait context/duration incorrect: %v", d)
		}
		waits++
		cancel()
		return false
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || waits != 1 || summary.Confirmed != 1 {
		t.Fatalf("failed status was treated as exact: summary=%+v err=%v calls=%d waits=%d", summary, err, calls, waits)
	}
}

func TestTranscriptStorageUsageReconcileSummaryCounts(t *testing.T) {
	var summary TranscriptStorageReconcileSummary
	result := TranscriptStorageReconcilePassResult{Corrected: 1, Confirmed: 2, Raced: 3, Busy: 4, Anomaly: 5, Timeout: 6, Error: 7, Skipped: 8, Entries: 9, LeaseHeld: true, MaxUnitElapsed: 10 * time.Second}
	summary.add(result)
	result.MaxUnitElapsed = time.Second
	summary.add(result)
	if summary.Passes != 2 || summary.LeaseHeldPasses != 2 || summary.Corrected != 2 || summary.Confirmed != 4 || summary.Raced != 6 || summary.Busy != 8 || summary.Anomaly != 10 || summary.Timeout != 12 || summary.Error != 14 || summary.Skipped != 16 || summary.Entries != 18 || summary.MaxUnitElapsed != 10*time.Second {
		t.Fatalf("summary aggregation = %+v", summary)
	}
}

func TestTranscriptStorageUsageReconcilePassDiscardedStatus(t *testing.T) {
	for _, readFails := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		runner := newTranscriptStorageReconcileRunner()
		for range 3 {
			runner.record(transcriptStorageReconcileUnit{"fixture", 0}, "timeout", time.Now())
		}
		result := TranscriptStorageReconcilePassResult{UnverifiedEligible: 9}
		closed, published := false, 0
		finishTranscriptStorageReconcilePass(ctx, &result, runner, true, func() { closed = true }, func(statusCtx context.Context) (TranscriptStorageReconcileStatus, error) {
			if !closed {
				t.Fatal("replacement status borrowed before pinned connection was closed")
			}
			deadline, ok := statusCtx.Deadline()
			if statusCtx.Err() != nil || !ok || time.Until(deadline) > 5*time.Second {
				t.Fatal("final status needs an independent bounded context")
			}
			if readFails {
				return TranscriptStorageReconcileStatus{}, errors.New("synthetic status failure")
			}
			return TranscriptStorageReconcileStatus{UnverifiedEligible: 7, UnverifiedDeferred: 2}, nil
		}, func(status TranscriptStorageReconcileStatus) {
			published++
			if status.Stuck != 1 || status.UnverifiedEligible != 7 {
				t.Fatalf("final callback = %+v", status)
			}
		})
		if readFails {
			if published != 0 || result.Err == nil || result.UnverifiedEligible != 9 {
				t.Fatalf("failed status published invented sample: %+v", result)
			}
		} else if published != 1 || result.Err != nil || result.UnverifiedEligible != 7 || result.UnverifiedDeferred != 2 {
			t.Fatalf("discarded final status = %+v, callbacks=%d", result, published)
		}
	}
}

func TestTranscriptStorageUsageReconcilePacingShortUnitTimeout(t *testing.T) {
	for _, unitTimeout := range []time.Duration{10 * time.Second, 60 * time.Second} {
		t.Run(unitTimeout.String(), func(t *testing.T) {
			cfg := DefaultTranscriptStorageReconcileConfig()
			cfg.UnitTimeout = unitTimeout
			cfg.DutyCycle = .5
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			ctx, cancel := context.WithDeadline(t.Context(), now.Add(unitTimeout))
			defer cancel()
			var total time.Duration
			const query = time.Millisecond
			err := transcriptStorageReconcilePace(ctx, query, cfg.DutyCycle, min(60*time.Second, cfg.UnitTimeout), func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
				total += d
				now = now.Add(d)
				return nil
			})
			if err != nil || total != query {
				t.Fatalf("accepted %s unit cannot pace a tiny query: slept=%s error=%v", cfg.UnitTimeout, total, err)
			}
		})
	}
}

func TestTranscriptStorageUsageReconcilePassSuccessfulStatusNotRepeated(t *testing.T) {
	result := TranscriptStorageReconcilePassResult{UnverifiedEligible: 2, UnverifiedDeferred: 3}
	closed := false
	finishTranscriptStorageReconcilePass(t.Context(), &result, newTranscriptStorageReconcileRunner(), false, func() { closed = true }, func(context.Context) (TranscriptStorageReconcileStatus, error) {
		t.Fatal("successful pass attempted a duplicate final status read")
		return TranscriptStorageReconcileStatus{}, nil
	}, func(TranscriptStorageReconcileStatus) { t.Fatal("successful pass published a duplicate final status") })
	if !closed || result.Err != nil || result.UnverifiedEligible != 2 || result.UnverifiedDeferred != 3 {
		t.Fatalf("successful finish closed=%v result=%+v", closed, result)
	}
}
