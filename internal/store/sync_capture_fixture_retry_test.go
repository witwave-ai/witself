package store

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/testenv"
)

func syncCaptureFixtureRetryBudget(work time.Duration) time.Duration {
	return min(60*time.Second, max(2*time.Second, 20*work))
}

type syncCaptureFixtureProbeClass string

const (
	captureProbeBeginFailed    syncCaptureFixtureProbeClass = "begin_failed"
	captureProbeTimeout        syncCaptureFixtureProbeClass = "timeout"
	captureProbeUnhealthy      syncCaptureFixtureProbeClass = "unhealthy"
	captureProbeMissingAccount syncCaptureFixtureProbeClass = "missing_account"
	captureProbeSafe           syncCaptureFixtureProbeClass = "safe"
	captureProbeUnsafe         syncCaptureFixtureProbeClass = "unsafe"
)

// The first admission is measured as fixture work too, so standalone activation
// callers have a local calibration even when they did not use the seed helper.
// Each probe is closed before a wait; the core supplies its calibrated context.
func retrySyncCaptureFixtureAdmission(ctx context.Context, st *Store, account string, activate bool, seedWork time.Duration, logf func(string, ...any), onRetry ...func(time.Duration, syncCaptureFixtureProbeClass)) (*SyncCaptureActivation, string) {
	failure := "capture fixture capability failed"
	if activate {
		failure = "capture fixture activation failed"
	}
	attempt := func(ctx context.Context) (*SyncCaptureActivation, error) {
		if activate {
			return st.ActivateSyncCapture(ctx, account, SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability})
		}
		return nil, st.EnableSyncWriter(ctx, account, SyncPreferencesPrincipal, SyncPreferencesCapability)
	}
	probe := func(ctx context.Context) syncCaptureFixtureProbeClass {
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			return captureProbeBeginFailed
		}
		// The core gives deadline expiry precedence over these observations.
		// False can also mean failed SQL or a missing account, not a horizon.
		safe := syncCaptureAccountSnapshotSafe(ctx, tx, account)
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, account).Scan(&exists)
		rollbackErr := tx.Rollback(ctx)
		if err != nil || rollbackErr != nil {
			return captureProbeUnhealthy
		}
		if !exists {
			return captureProbeMissingAccount
		}
		if safe {
			return captureProbeSafe
		}
		return captureProbeUnsafe
	}
	return retrySyncCaptureFixtureAttempts(ctx, seedWork, failure, attempt, probe, logf, onRetry...)
}

// Explicit callbacks exercise admission, probe deadlines and diagnostics without
// database-wide horizon assumptions or mutable package globals. Real fixtures use
// the adapter above. The named result funnels every failure through one log site.
// A safe probe retries unless the immediately preceding probe was also safe:
// only two safe observations bracketing a refused admission rule out a cleared horizon.
func retrySyncCaptureFixtureAttempts(ctx context.Context, seedWork time.Duration, failure string, attempt func(context.Context) (*SyncCaptureActivation, error), probe func(context.Context) syncCaptureFixtureProbeClass, logf func(string, ...any), onRetry ...func(time.Duration, syncCaptureFixtureProbeClass)) (activation *SyncCaptureActivation, resultFailure string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	classes := make(map[syncCaptureFixtureProbeClass]int)
	var last syncCaptureFixtureProbeClass
	defer func() {
		if resultFailure != "" {
			logf("capture fixture probe classes begin_failed=%d timeout=%d unhealthy=%d missing_account=%d safe=%d unsafe=%d", classes[captureProbeBeginFailed], classes[captureProbeTimeout], classes[captureProbeUnhealthy], classes[captureProbeMissingAccount], classes[captureProbeSafe], classes[captureProbeUnsafe])
		}
	}()
	retries := 0
	budget := 60 * time.Second
	var probeBudget time.Duration
	for {
		a, err := attempt(ctx)
		if err == nil {
			if retries != 0 {
				logf("capture fixture admitted after foreign snapshot horizon cleared retries=%d", retries)
			}
			return a, ""
		}
		if retries == 0 {
			work := seedWork + time.Since(started)
			budget = syncCaptureFixtureRetryBudget(work)
			probeBudget = max(time.Second, 5*work)
			var calibratedCancel context.CancelFunc
			ctx, calibratedCancel = context.WithDeadline(ctx, started.Add(budget))
			defer calibratedCancel()
		}
		if ctx.Err() != nil {
			return nil, "capture fixture foreign snapshot horizon persisted"
		}
		deadline, _ := ctx.Deadline()
		probeCtx, probeCancel := context.WithTimeout(ctx, min(probeBudget, time.Until(deadline)))
		previous := last
		last = probe(probeCtx)
		if probeCtx.Err() == context.DeadlineExceeded {
			last = captureProbeTimeout
		}
		probeCancel()
		classes[last]++
		if ctx.Err() != nil {
			return nil, "capture fixture foreign snapshot horizon persisted"
		}
		switch last {
		case captureProbeBeginFailed, captureProbeTimeout, captureProbeUnsafe:
			// Inconclusive observations retain the calibrated retry allowance.
		case captureProbeSafe:
			if previous == captureProbeSafe {
				return nil, failure
			}
		case captureProbeUnhealthy, captureProbeMissingAccount:
			return nil, failure
		default:
			panic("capture fixture unknown probe class")
		}
		for _, observe := range onRetry {
			observe(budget, last)
		}
		select {
		case <-ctx.Done():
			return nil, "capture fixture foreign snapshot horizon persisted"
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return nil, "capture fixture foreign snapshot horizon persisted"
		}
		retries++
	}
}

func TestSyncCaptureFixtureRetryBudget(t *testing.T) {
	for _, tc := range []struct{ work, want time.Duration }{
		{0, 2 * time.Second},
		{150 * time.Millisecond, 3 * time.Second},
		{4 * time.Second, 60 * time.Second},
	} {
		if got := syncCaptureFixtureRetryBudget(tc.work); got != tc.want {
			t.Fatalf("capture fixture proportional budget differs: got=%s want=%s", got, tc.want)
		}
	}
}

func requireSyncCaptureFixtureSuccessLog(t *testing.T, logs []string, retries int) {
	t.Helper()
	if retries == 0 {
		if len(logs) != 0 {
			t.Fatal("capture fixture first-try admission must be silent")
		}
		return
	}
	if len(logs) == 0 {
		t.Fatal("capture retry success log missing")
	}
	if len(logs) > 1 {
		t.Fatal("capture fixture success line emitted more than once")
	}
	if logs[0] != fmt.Sprintf("capture fixture admitted after foreign snapshot horizon cleared retries=%d", retries) {
		t.Fatal("capture retry success log missing retry count")
	}
}

func TestSyncCaptureFixtureRetryLogging(t *testing.T) {
	for _, tc := range []struct {
		name     string
		refusals int
	}{
		{name: "first-try", refusals: 0},
		{name: "retry-success-exactly-once", refusals: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, probes, retries := 0, 0, 0
			var logs []string
			want := &SyncCaptureActivation{}
			a, failure := retrySyncCaptureFixtureAttempts(context.Background(), 0, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) {
				attempts++
				if attempts <= tc.refusals {
					return nil, ErrSyncWriterNotAdmitted
				}
				return want, nil
			}, func(context.Context) syncCaptureFixtureProbeClass {
				probes++
				return captureProbeUnsafe
			}, func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}, func(_ time.Duration, class syncCaptureFixtureProbeClass) {
				retries++
				if class != captureProbeUnsafe {
					t.Error("capture fixture fake horizon observation differs")
				}
			})
			if failure != "" || a != want {
				t.Fatal("capture fixture fake admission failed")
			}
			if attempts != tc.refusals+1 || probes != tc.refusals || retries != tc.refusals {
				t.Fatal("capture fixture fake admission counts differ")
			}
			requireSyncCaptureFixtureSuccessLog(t, logs, retries)
		})
	}
}

func TestSyncCaptureFixtureRetrySafeBracket(t *testing.T) {
	for _, failure := range []string{"capture fixture capability failed", "capture fixture activation failed"} {
		for _, tc := range []struct {
			name    string
			classes []syncCaptureFixtureProbeClass
			refuse  bool
		}{
			{name: "unsafe-safe-admitted", classes: []syncCaptureFixtureProbeClass{captureProbeUnsafe, captureProbeSafe}},
			{name: "safe-safe-refused", classes: []syncCaptureFixtureProbeClass{captureProbeSafe, captureProbeSafe}, refuse: true},
			{name: "safe-admitted", classes: []syncCaptureFixtureProbeClass{captureProbeSafe}},
			{name: "safe-unsafe-safe-admitted", classes: []syncCaptureFixtureProbeClass{captureProbeSafe, captureProbeUnsafe, captureProbeSafe}},
			{name: "safe-timeout-safe-admitted", classes: []syncCaptureFixtureProbeClass{captureProbeSafe, captureProbeTimeout, captureProbeSafe}},
			{name: "safe-begin-failed-safe-admitted", classes: []syncCaptureFixtureProbeClass{captureProbeSafe, captureProbeBeginFailed, captureProbeSafe}},
		} {
			t.Run(failure+"/"+tc.name, func(t *testing.T) {
				attempts, probes, retries := 0, 0, 0
				var logs []string
				want := &SyncCaptureActivation{}
				a, got := retrySyncCaptureFixtureAttempts(context.Background(), 0, failure, func(context.Context) (*SyncCaptureActivation, error) {
					attempts++
					if tc.refuse || attempts <= len(tc.classes) {
						return nil, ErrSyncWriterNotAdmitted
					}
					return want, nil
				}, func(context.Context) syncCaptureFixtureProbeClass {
					class := tc.classes[min(probes, len(tc.classes)-1)]
					probes++
					return class
				}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(_ time.Duration, class syncCaptureFixtureProbeClass) {
					if retries >= len(tc.classes) || class != tc.classes[retries] {
						t.Error("capture fixture safe bracket retry observation differs")
					}
					retries++
				})
				if tc.refuse {
					if a != nil || got != failure {
						t.Fatal("capture fixture safe bracket must return generic refusal")
					}
					if attempts != 2 || probes != 2 || retries != 1 {
						t.Fatal("capture fixture safe bracket must refuse after exactly two attempts")
					}
					if len(logs) != 1 || logs[0] != "capture fixture probe classes begin_failed=0 timeout=0 unhealthy=0 missing_account=0 safe=2 unsafe=0" {
						t.Fatal("capture fixture safe bracket failure summary differs")
					}
					return
				}
				if got != "" || a != want {
					t.Fatal("capture fixture safe probe must reattempt admission")
				}
				if attempts != len(tc.classes)+1 || probes != len(tc.classes) || retries != len(tc.classes) {
					t.Fatal("capture fixture safe bracket admission counts differ")
				}
				requireSyncCaptureFixtureSuccessLog(t, logs, retries)
			})
		}
	}
}

func TestSyncCaptureFixtureRetryUnsafeExhaustion(t *testing.T) {
	attempts, probes, retries := 0, 0, 0
	var logs []string
	a, failure := retrySyncCaptureFixtureAttempts(context.Background(), 0, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) {
		attempts++
		return nil, ErrSyncWriterNotAdmitted
	}, func(context.Context) syncCaptureFixtureProbeClass {
		probes++
		return captureProbeUnsafe
	}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(time.Duration, syncCaptureFixtureProbeClass) { retries++ })
	if a != nil || failure != "capture fixture foreign snapshot horizon persisted" {
		t.Fatal("capture fixture unsafe exhaustion literal differs")
	}
	if attempts < 2 || probes < 2 || retries < 2 {
		t.Fatal("capture fixture unsafe exhaustion must retry within budget")
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "capture fixture probe classes ") {
		t.Fatal("capture fixture unsafe exhaustion summary differs")
	}
}

func TestSyncCaptureFixtureRetryPostgres(t *testing.T) {
	for _, activate := range []bool{false, true} {
		name := map[bool]string{false: "enable", true: "activate"}[activate]
		for _, release := range []bool{true, false} {
			scenario := map[bool]string{true: "released", false: "persisted"}[release]
			t.Run(name+"/"+scenario, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute+testenv.MigrationBudget(0))
				defer cancel()
				var old pgx.Tx
				var pid uint32
				st, p, work := seedSyncCaptureFixture(t, func(st *Store) {
					var err error
					old, err = st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
					if err != nil {
						t.Fatal("capture retry pin begin failed")
					}
					t.Cleanup(func() { _ = old.Rollback(context.Background()) })
					var one int
					if old.QueryRow(ctx, `SELECT 1`).Scan(&one) != nil || old.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid) != nil {
						t.Fatal("capture retry pin snapshot failed")
					}
				})
				if activate {
					// Isolate activation from the earlier enable admission.
					if _, err := st.pool.Exec(ctx, `INSERT INTO sync_writer_capabilities(account_id,principal,capability,manifest_version,enabled,capture_epoch) VALUES($1,$2,$3,$4,true,1)`, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability, SyncCoverageManifestVersion); err != nil {
						t.Fatal("capture retry activation capability seed failed")
					}
				}
				capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity activity JOIN accounts account ON account.id=$1 WHERE activity.pid=$2 AND activity.backend_xmin IS NOT NULL AND age(activity.backend_xmin)>=age(account.xmin))`, p.AccountID, pid)
				var logs []string // Read only after result joins the admission goroutine.
				result := make(chan string, 1)
				type retryObservation struct{ budget, observed time.Duration }
				retrying := make(chan retryObservation, 1)
				releaseRequested := make(chan struct{})
				paused := make(chan struct{}, 1)
				resume := make(chan struct{})
				started := time.Now()
				go func() {
					a, failure := retrySyncCaptureFixtureAdmission(ctx, st, p.AccountID, activate, work, func(format string, args ...any) {
						logs = append(logs, fmt.Sprintf(format, args...))
						t.Logf(format, args...)
					}, func(budget time.Duration, class syncCaptureFixtureProbeClass) {
						if class != captureProbeUnsafe {
							return
						}
						select {
						case retrying <- retryObservation{budget: budget, observed: time.Since(started)}:
						default:
						}
						select {
						case <-releaseRequested:
							select {
							case paused <- struct{}{}:
							default:
							}
							select {
							case <-resume:
							case <-ctx.Done():
							}
						default:
						}
					})
					if a != nil {
						a.Close()
					}
					result <- failure
				}()
				// Always cancel and join before the schema cleanup, even when an
				// assertion fails while the foreign snapshot is still held.
				joined := false
				defer func() {
					cancel()
					if !joined {
						<-result
					}
				}()
				var observation retryObservation
				select {
				case observation = <-retrying:
				case failure := <-result:
					joined = true
					t.Fatalf("capture retry ended before horizon observation: %s", failure)
				case <-ctx.Done():
					t.Fatal("capture retry did not observe foreign horizon")
				}
				// This interval includes the first admission and its probe, so it
				// bounds the helper's admission sample without trusting its budget.
				expected := syncCaptureFixtureRetryBudget(work + observation.observed)
				t.Logf("capture fixture retry calibration work=%s observed=%s expected=%s", work, observation.observed, expected)
				if observation.budget > expected {
					t.Fatal("capture fixture retry budget exceeds the proportional bound")
				}
				holdStarted := time.Now()
				// Independent of the budget function (M4 must not shorten this).
				// Poll throughout a proportional hold that is always >100ms.
				hold := max(250*time.Millisecond, work)
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for time.Since(holdStarted) < hold {
					select {
					case failure := <-result:
						joined = true
						t.Fatalf("capture retry ended while foreign snapshot held: %s", failure)
					case <-ctx.Done():
						t.Fatal("capture retry hold observation timed out")
					case <-ticker.C:
					}
				}
				if release {
					// Release between the completed unsafe probe and the next
					// attempt. A safe probe after a refusal also permits another
					// admission unless the preceding probe was already safe.
					close(releaseRequested)
					select {
					case <-paused:
					case failure := <-result:
						joined = true
						t.Fatalf("capture retry ended before synchronized release: %s", failure)
					case <-ctx.Done():
						t.Fatal("capture retry release observation timed out")
					}
					if old.Rollback(ctx) != nil {
						t.Fatal("capture retry pin release failed")
					}
					close(resume)
				}
				// Allow scheduler/cleanup overhead proportional to measured work;
				// the helper's hard cap is 60s, including its first admission.
				bound := expected + max(time.Second, 5*work)
				wait, waitCancel := context.WithDeadline(ctx, started.Add(bound))
				defer waitCancel()
				select {
				case failure := <-result:
					joined = true
					if release {
						if failure != "" {
							t.Fatalf("capture retry did not admit after horizon release: %s", failure)
						}
						if len(logs) == 0 {
							t.Fatal("capture retry success log missing")
						}
						if len(logs) > 1 {
							t.Fatal("capture fixture success line emitted more than once")
						}
						line := logs[0]
						if !strings.HasPrefix(line, "capture fixture admitted after foreign snapshot horizon cleared retries=") || strings.HasSuffix(line, "retries=0") {
							t.Fatal("capture retry success log missing retry count")
						}
					} else {
						if failure != "capture fixture foreign snapshot horizon persisted" {
							t.Fatalf("capture retry exhaustion literal differs: %q", failure)
						}
					}
				case <-wait.Done():
					t.Fatal("capture retry exceeded proportional completion bound")
				}
			})
		}
	}
}

func TestSyncCaptureFixtureRetryFirstTryPostgres(t *testing.T) {
	for _, activate := range []bool{false, true} {
		name := map[bool]string{false: "enable", true: "activate"}[activate]
		t.Run(name, func(t *testing.T) {
			// No test-owned snapshot does not imply database-wide quiet.
			st, p, work := seedSyncCaptureFixture(t, nil)
			ctx := context.Background()
			if activate {
				// Seed the capability directly so this call tests activation's
				// own first attempt, independently of the enable wrapper.
				if _, err := st.pool.Exec(ctx, `INSERT INTO sync_writer_capabilities(account_id,principal,capability,manifest_version,enabled,capture_epoch) VALUES($1,$2,$3,$4,true,1)`, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability, SyncCoverageManifestVersion); err != nil {
					t.Fatal("capture retry activation capability seed failed")
				}
			}
			var logs []string
			retries := 0
			a, failure := retrySyncCaptureFixtureAdmission(ctx, st, p.AccountID, activate, work, func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
				t.Logf(format, args...)
			}, func(time.Duration, syncCaptureFixtureProbeClass) { retries++ })
			if a != nil {
				a.Close()
			}
			if failure != "" {
				t.Fatalf("capture fixture first-try admission failed: %s", failure)
			}
			t.Logf("capture fixture first-try sample retries=%d", retries)
			requireSyncCaptureFixtureSuccessLog(t, logs, retries)
		})
	}
}

func TestSyncCaptureFixtureRetryOverlapPostgres(t *testing.T) {
	for _, activate := range []bool{false, true} {
		name := map[bool]string{false: "enable", true: "activate"}[activate]
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute+testenv.MigrationBudget(0))
			defer cancel()
			var old pgx.Tx
			st, p, work := seedSyncCaptureFixture(t, func(st *Store) {
				var err error
				old, err = st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
				if err != nil {
					t.Fatal("capture retry overlap pin begin failed")
				}
				t.Cleanup(func() { _ = old.Rollback(context.Background()) })
				var one int
				if old.QueryRow(ctx, `SELECT 1`).Scan(&one) != nil {
					t.Fatal("capture retry overlap pin snapshot failed")
				}
			})
			if activate {
				if _, err := st.pool.Exec(ctx, `INSERT INTO sync_writer_capabilities(account_id,principal,capability,manifest_version,enabled,capture_epoch) VALUES($1,$2,$3,$4,true,1)`, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability, SyncCoverageManifestVersion); err != nil {
					t.Fatal("capture retry activation capability seed failed")
				}
			}
			pollCtx, pollCancel := context.WithTimeout(ctx, max(time.Second, 5*work))
			defer pollCancel()
			pid := old.Conn().PgConn().PID()
			captureObserve(pollCtx, t, capturePoolQuery(st, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity activity JOIN accounts account ON account.id=$1 WHERE activity.pid=$2 AND activity.backend_xmin IS NOT NULL AND age(activity.backend_xmin)>=age(account.xmin))`, p.AccountID, pid))
			var logs []string // Read after joining the admission goroutine.
			retries := 0
			type retryObservation struct{ budget, observed time.Duration }
			unsafe := make(chan retryObservation, 1)
			resume := make(chan struct{})
			result := make(chan string, 1)
			started := time.Now()
			go func() {
				observedUnsafe := false
				a, failure := retrySyncCaptureFixtureAdmission(ctx, st, p.AccountID, activate, work, func(format string, args ...any) {
					logs = append(logs, fmt.Sprintf(format, args...))
					t.Logf(format, args...)
				}, func(budget time.Duration, class syncCaptureFixtureProbeClass) {
					retries++
					if class != captureProbeUnsafe || observedUnsafe {
						return
					}
					observedUnsafe = true
					unsafe <- retryObservation{budget: budget, observed: time.Since(started)}
					select {
					case <-resume:
					case <-ctx.Done():
					}
				})
				if a != nil {
					a.Close()
				}
				result <- failure
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-result
				}
			}()
			var observation retryObservation
			select {
			case observation = <-unsafe:
			case failure := <-result:
				joined = true
				t.Fatalf("capture retry overlap ended before foreign horizon observation: %s", failure)
			case <-ctx.Done():
				t.Fatal("capture retry overlap did not observe foreign horizon")
			}
			expected := syncCaptureFixtureRetryBudget(work + observation.observed)
			t.Logf("capture fixture overlap calibration work=%s observed=%s expected=%s", work, observation.observed, expected)
			if observation.budget > expected {
				t.Fatal("capture fixture retry budget exceeds the proportional bound")
			}
			// The observer pauses at the first unsafe probe until the pinned
			// connection is released; the next tick can then retry admission.
			if old.Rollback(ctx) != nil {
				t.Fatal("capture retry overlap pin release failed")
			}
			wait, waitCancel := context.WithDeadline(ctx, started.Add(expected+max(time.Second, 5*work)))
			defer waitCancel()
			// The pool may reuse the released connection for this observation.
			captureObserve(wait, t, capturePoolQuery(st, `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND pid<>pg_backend_pid() AND backend_xmin IS NOT NULL)`, pid))
			close(resume)
			var failure string
			select {
			case failure = <-result:
				joined = true
			case <-wait.Done():
				t.Fatal("capture retry exceeded proportional completion bound")
			}
			if failure != "" {
				t.Fatalf("capture retry overlap did not admit after horizon release: %s", failure)
			}
			if retries == 0 {
				t.Fatal("capture retry overlap did not observe foreign horizon")
			}
			if time.Since(started) > expected+max(time.Second, 5*work) {
				t.Fatal("capture retry exceeded proportional completion bound")
			}
			requireSyncCaptureFixtureSuccessLog(t, logs, retries)
		})
	}
}

func TestSyncCaptureFixtureRetryOtherCausePostgres(t *testing.T) {
	for _, activate := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("activate=%t/missing=%t", activate, missing), func(t *testing.T) {
				st, p, work := seedSyncCaptureFixture(t, nil)
				account := p.AccountID
				if missing {
					account = "missing_capture_retry_account"
				} else if !activate {
					// Existing account, safe horizon, refused for selection.
					st.syncCaptureSelected.Store(false)
				}
				// The existing account lacks a capability for activation, so
				// its predicate is safe but the admission must still fail.
				ctx := context.Background()
				var logs []string
				invalidRetries, horizonRetries, safeRetries := 0, 0, 0
				started := time.Now()
				a, failure := retrySyncCaptureFixtureAdmission(ctx, st, account, activate, work, func(format string, args ...any) {
					logs = append(logs, fmt.Sprintf(format, args...))
					t.Logf(format, args...)
				}, func(_ time.Duration, class syncCaptureFixtureProbeClass) {
					if class == captureProbeSafe {
						safeRetries++
					} else if class != captureProbeUnsafe && class != captureProbeTimeout && class != captureProbeBeginFailed {
						invalidRetries++
					} else {
						horizonRetries++
					}
				})
				if a != nil {
					a.Close()
					t.Fatal("capture retry admitted another cause")
				}
				want := "capture fixture capability failed"
				if activate {
					want = "capture fixture activation failed"
				}
				if len(logs) != 1 || !strings.HasPrefix(logs[0], "capture fixture probe classes ") {
					t.Fatal("capture fixture failure classes line differs")
				}
				if invalidRetries != 0 {
					t.Fatal("capture retry other cause did not fail immediately")
				}
				if failure != want {
					t.Fatalf("capture retry other cause literal differs: %q", failure)
				}
				if (missing && safeRetries != 0) || (!missing && safeRetries == 0) {
					t.Fatal("capture retry other cause safe bracket differs")
				}
				// An unsafe horizon or inconclusive probe can overlap an unrelated
				// refusal. Immediate timing applies only when neither was retried.
				if horizonRetries == 0 && time.Since(started) >= max(time.Second, 10*work) {
					t.Fatal("capture retry other cause did not fail immediately")
				}
			})
		}
	}
}

func TestSyncCaptureFixtureAdmissionSourceContract(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "sync_capture_integration_test.go", nil, 0)
	if err != nil {
		t.Fatal("capture fixture source parse failed")
	}
	seen := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "newSyncCaptureFixture" && fn.Name.Name != "activateSyncFixture" && fn.Name.Name != "seedSyncCaptureFixture") {
			continue
		}
		seen++
		helperCalls := 0
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if method, ok := call.Fun.(*ast.SelectorExpr); ok && (method.Sel.Name == "EnableSyncWriter" || method.Sel.Name == "ActivateSyncCapture") {
				t.Errorf("capture fixture admission bypasses retry helper: %s", fn.Name.Name)
			}
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "retrySyncCaptureFixtureAdmission" {
				helperCalls++
			}
			return true
		})
		want := 1
		if fn.Name.Name == "seedSyncCaptureFixture" {
			want = 0
		}
		if helperCalls != want {
			t.Errorf("capture fixture retry helper call count differs: %s", fn.Name.Name)
		}
	}
	if seen != 3 {
		t.Fatal("capture fixture source contract lost an admission wrapper")
	}
	requireSyncCaptureFixtureFirstTrySampleSourceContract(t)
}

// Call this at the end of TestSyncCaptureFixtureAdmissionSourceContract.
// AST positions are independent of LF versus CRLF source line endings.
func requireSyncCaptureFixtureFirstTrySampleSourceContract(t *testing.T) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "sync_capture_fixture_retry_test.go", nil, 0)
	if err != nil {
		t.Fatal("capture fixture first-try source parse failed")
	}
	var firstTry *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestSyncCaptureFixtureRetryFirstTryPostgres" {
			firstTry = fn
		}
	}
	if firstTry == nil {
		t.Fatal("capture fixture first-try source contract lost test")
	}
	isIdent := func(expr ast.Expr, want string) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && ident.Name == want
	}
	var bodies []*ast.BlockStmt
	ast.Inspect(firstTry.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isIdent(method.X, "t") || method.Sel.Name != "Run" {
			return true
		}
		if subtest, ok := call.Args[1].(*ast.FuncLit); ok {
			bodies = append(bodies, subtest.Body)
		}
		return false
	})
	if len(bodies) != 1 {
		t.Fatal("capture fixture first-try source contract lost subtest")
	}
	body := bodies[0].List
	admission, sample := -1, -1
	admissionCount, sampleCount := 0, 0
	for i, stmt := range body {
		if assignment, ok := stmt.(*ast.AssignStmt); ok {
			for _, rhs := range assignment.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok && isIdent(call.Fun, "retrySyncCaptureFixtureAdmission") {
					admission, admissionCount = i, admissionCount+1
				}
			}
		}
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			continue
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isIdent(method.X, "t") || method.Sel.Name != "Logf" || !isIdent(call.Args[1], "retries") {
			continue
		}
		format, ok := call.Args[0].(*ast.BasicLit)
		if ok && format.Kind == token.STRING && format.Value == `"capture fixture first-try sample retries=%d"` {
			sample, sampleCount = i, sampleCount+1
		}
	}
	if admissionCount != 1 || sampleCount != 1 || sample <= admission {
		t.Fatal("capture fixture first-try sample must follow admission")
	}
	for _, stmt := range body[admission+1 : sample] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			// A callback's return does not leave this subtest. A return in
			// an if, switch or other block does and can suppress the sample.
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			if _, ok := n.(*ast.ReturnStmt); ok {
				t.Error("capture fixture first-try sample bypassed by return")
			}
			return true
		})
	}
	if sample+1 >= len(body) {
		t.Fatal("capture fixture first-try sample must be followed by success-log assertion")
	}
	expr, ok := body[sample+1].(*ast.ExprStmt)
	if !ok {
		t.Fatal("capture fixture first-try sample must be followed by success-log assertion")
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok || !isIdent(call.Fun, "requireSyncCaptureFixtureSuccessLog") || len(call.Args) != 3 || !isIdent(call.Args[0], "t") || !isIdent(call.Args[1], "logs") || !isIdent(call.Args[2], "retries") {
		t.Fatal("capture fixture first-try sample must be followed by success-log assertion")
	}
}

func TestSyncCaptureFixtureProbeDiagnosticGuard(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "sync_capture_fixture_retry_test.go", nil, 0)
	if err != nil {
		t.Fatal("capture fixture probe diagnostic source parse failed")
	}
	want := map[string]string{
		"captureProbeBeginFailed":    `"begin_failed"`,
		"captureProbeTimeout":        `"timeout"`,
		"captureProbeUnhealthy":      `"unhealthy"`,
		"captureProbeMissingAccount": `"missing_account"`,
		"captureProbeSafe":           `"safe"`,
		"captureProbeUnsafe":         `"unsafe"`,
	}
	isIdent := func(expr ast.Expr, name string) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && ident.Name == name
	}
	seen := make(map[string]bool)
	for _, decl := range f.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		inheritedClass := false
		for _, spec := range group.Specs {
			value := spec.(*ast.ValueSpec)
			class := isIdent(value.Type, "syncCaptureFixtureProbeClass")
			if value.Type == nil && len(value.Values) == 0 {
				class = inheritedClass
			}
			// An alias or conversion also creates a typed class constant.
			for _, expr := range value.Values {
				ast.Inspect(expr, func(n ast.Node) bool {
					if ident, ok := n.(*ast.Ident); ok {
						if _, known := want[ident.Name]; known || ident.Name == "syncCaptureFixtureProbeClass" {
							class = true
						}
					}
					return true
				})
			}
			inheritedClass = class
			if !class {
				continue
			}
			if len(value.Names) != 1 || len(value.Values) != 1 || !isIdent(value.Type, "syncCaptureFixtureProbeClass") {
				t.Fatal("capture fixture probe vocabulary differs")
			}
			name := value.Names[0].Name
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || want[name] == "" || want[name] != literal.Value || seen[name] {
				t.Fatal("capture fixture probe vocabulary differs")
			}
			seen[name] = true
		}
	}
	if len(seen) != len(want) {
		t.Fatal("capture fixture probe vocabulary differs")
	}
	format := `"capture fixture probe classes begin_failed=%d timeout=%d unhealthy=%d missing_account=%d safe=%d unsafe=%d"`
	order := []string{"captureProbeBeginFailed", "captureProbeTimeout", "captureProbeUnhealthy", "captureProbeMissingAccount", "captureProbeSafe", "captureProbeUnsafe"}
	lines := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "retrySyncCaptureFixtureAttempts" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isIdent(call.Fun, "logf") || len(call.Args) == 0 {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || !strings.HasPrefix(literal.Value, `"capture fixture probe classes`) {
				return true
			}
			lines++
			if literal.Value != format || len(call.Args) != len(order)+1 {
				t.Fatal("capture fixture probe classes diagnostic differs")
			}
			for i, name := range order {
				index, ok := call.Args[i+1].(*ast.IndexExpr)
				if !ok || !isIdent(index.X, "classes") || !isIdent(index.Index, name) {
					t.Fatal("capture fixture probe classes diagnostic differs")
				}
			}
			return true
		})
	}
	if lines != 1 {
		t.Fatal("capture fixture probe classes diagnostic differs")
	}
}
