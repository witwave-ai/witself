package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSyncCaptureFixtureProbeRetry(t *testing.T) {
	for _, inconclusive := range []syncCaptureFixtureProbeClass{captureProbeTimeout, captureProbeBeginFailed} {
		t.Run(string(inconclusive)+"-then-unsafe", func(t *testing.T) {
			attempts, probes, retries := 0, 0, 0
			var logs []string
			want := &SyncCaptureActivation{}
			a, failure := retrySyncCaptureFixtureAttempts(context.Background(), 0, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) {
				attempts++
				if attempts <= 4 {
					return nil, ErrSyncWriterNotAdmitted
				}
				return want, nil
			}, func(context.Context) syncCaptureFixtureProbeClass {
				probes++
				if probes <= 3 {
					return inconclusive
				}
				return captureProbeUnsafe
			}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(_ time.Duration, class syncCaptureFixtureProbeClass) {
				retries++
				if (retries <= 3 && class != inconclusive) || (retries == 4 && class != captureProbeUnsafe) {
					t.Error("capture fixture probe retry class differs")
				}
			})
			if failure != "" || a != want {
				t.Fatal("capture fixture inconclusive probe did not retry to admission")
			}
			if attempts != 5 || probes != 4 || retries != 4 {
				t.Fatal("capture fixture inconclusive probe retry count differs")
			}
			requireSyncCaptureFixtureSuccessLog(t, logs, retries)
		})
	}
}

func TestSyncCaptureFixtureProbeFailure(t *testing.T) {
	for _, class := range []syncCaptureFixtureProbeClass{captureProbeBeginFailed, captureProbeTimeout, captureProbeUnsafe, captureProbeUnhealthy, captureProbeMissingAccount, captureProbeSafe} {
		for _, failure := range []string{"capture fixture capability failed", "capture fixture activation failed"} {
			t.Run(string(class)+"/"+failure, func(t *testing.T) {
				attempts, probes, retries := 0, 0, 0
				var logs []string
				a, got := retrySyncCaptureFixtureAttempts(context.Background(), 0, failure, func(context.Context) (*SyncCaptureActivation, error) {
					attempts++
					return nil, ErrSyncWriterNotAdmitted
				}, func(context.Context) syncCaptureFixtureProbeClass { probes++; return class }, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(time.Duration, syncCaptureFixtureProbeClass) { retries++ })
				if class == captureProbeSafe {
					if attempts != 2 || probes != 2 || retries != 1 {
						t.Fatal("capture fixture safe probe bracket counts differ")
					}
					if a != nil || got != failure {
						t.Fatal("capture fixture safe probe bracket refusal differs")
					}
					if len(logs) != 1 || logs[0] != "capture fixture probe classes begin_failed=0 timeout=0 unhealthy=0 missing_account=0 safe=2 unsafe=0" {
						t.Fatal("capture fixture safe probe bracket classes line must appear exactly once with exact counts")
					}
					t.Log("capture fixture safe probe outcome: bracketed refusal")
					return
				}
				want := failure
				if class == captureProbeBeginFailed || class == captureProbeTimeout || class == captureProbeUnsafe {
					want = "capture fixture foreign snapshot horizon persisted"
					if retries == 0 || probes == 0 {
						t.Fatal("capture fixture inconclusive probe failed before retry budget")
					}
				} else if retries != 0 || attempts != 1 || probes != 1 {
					t.Fatal("capture fixture conclusive probe did not fail immediately")
				}
				if a != nil || got != want {
					t.Fatal("capture fixture probe exhaustion or refusal literal differs")
				}
				if len(logs) != 1 {
					t.Fatal("capture fixture failure probe classes line must appear exactly once with exact counts")
				}
				format := "capture fixture probe classes begin_failed=%d timeout=%d unhealthy=%d missing_account=%d safe=%d unsafe=%d"
				var counts [6]int
				n, err := fmt.Sscanf(logs[0], format, &counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5])
				if err != nil || n != len(counts) || logs[0] != fmt.Sprintf(format, counts[0], counts[1], counts[2], counts[3], counts[4], counts[5]) {
					t.Fatal("capture fixture failure probe classes line must appear exactly once with exact counts")
				}
				total := 0
				for i, observed := range []syncCaptureFixtureProbeClass{captureProbeBeginFailed, captureProbeTimeout, captureProbeUnhealthy, captureProbeMissingAccount, captureProbeSafe, captureProbeUnsafe} {
					// Any immediate fake can cross its child deadline. Accept
					// normalization to timeout only on the repeated-probe paths.
					allowed := observed == class || (want != failure && observed == captureProbeTimeout)
					if counts[i] < 0 || (!allowed && counts[i] != 0) {
						t.Fatal("capture fixture failure probe classes counts differ")
					}
					total += counts[i]
				}
				if total != probes {
					t.Fatal("capture fixture failure probe classes counts differ")
				}
			})
		}
	}
	t.Run("expired-before-probe", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var logs []string
		_, failure := retrySyncCaptureFixtureAttempts(ctx, 0, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) { return nil, ErrSyncWriterNotAdmitted }, func(context.Context) syncCaptureFixtureProbeClass {
			t.Error("capture fixture probed expired budget")
			return captureProbeSafe
		}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
		if failure != "capture fixture foreign snapshot horizon persisted" {
			t.Fatal("capture fixture expired budget literal differs")
		}
		if len(logs) != 1 || logs[0] != "capture fixture probe classes begin_failed=0 timeout=0 unhealthy=0 missing_account=0 safe=0 unsafe=0" {
			t.Fatal("capture fixture failure probe classes line must appear exactly once with exact counts")
		}
	})
}

func TestSyncCaptureFixtureProbeDeadline(t *testing.T) {
	t.Run("timeout-before-outer-budget", func(t *testing.T) {
		attempts, retries := 0, 0
		var logs []string
		_, failure := retrySyncCaptureFixtureAttempts(context.Background(), time.Second, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) {
			attempts++
			if attempts == 1 {
				return nil, ErrSyncWriterNotAdmitted
			}
			return nil, nil
		}, func(ctx context.Context) syncCaptureFixtureProbeClass {
			<-ctx.Done()
			return captureProbeUnhealthy
		}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(_ time.Duration, class syncCaptureFixtureProbeClass) {
			retries++
			if class != captureProbeTimeout {
				t.Error("capture fixture expired probe class differs")
			}
		})
		if failure != "" || attempts != 2 || retries != 1 {
			t.Fatal("capture fixture expired probe did not retry within outer budget")
		}
		requireSyncCaptureFixtureSuccessLog(t, logs, retries)
	})
	t.Run("proportional-latency", func(t *testing.T) {
		// Model probe latency at twice the seed measurement. The supplied
		// deadline must cover it; no scheduler-sensitive sleep is needed.
		seed := time.Second
		attempts, probes, retries := 0, 0, 0
		concluded := false
		var logs []string
		_, failure := retrySyncCaptureFixtureAttempts(context.Background(), seed, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) {
			attempts++
			if attempts == 1 {
				return nil, ErrSyncWriterNotAdmitted
			}
			return nil, nil
		}, func(ctx context.Context) syncCaptureFixtureProbeClass {
			probes++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 2*seed {
				return captureProbeTimeout
			}
			concluded = true
			return captureProbeUnsafe
		}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, func(time.Duration, syncCaptureFixtureProbeClass) { retries++ })
		if !concluded {
			t.Fatal("capture fixture proportional probe could not conclude modeled seed latency")
		}
		if failure != "" || probes != 1 || attempts != 2 || retries != 1 {
			t.Fatal("capture fixture proportional probe admission differs")
		}
		requireSyncCaptureFixtureSuccessLog(t, logs, retries)
	})
	t.Run("remaining-budget", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		outer, _ := ctx.Deadline()
		var logs []string
		probes := 0
		_, failure := retrySyncCaptureFixtureAttempts(ctx, time.Second, "capture fixture capability failed", func(context.Context) (*SyncCaptureActivation, error) { return nil, ErrSyncWriterNotAdmitted }, func(ctx context.Context) syncCaptureFixtureProbeClass {
			probes++
			deadline, ok := ctx.Deadline()
			if !ok || deadline.After(outer) {
				t.Error("capture fixture probe exceeds remaining budget")
			}
			<-ctx.Done()
			// A failed query on an expired probe must be recorded as timeout.
			return captureProbeUnhealthy
		}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
		if failure != "capture fixture foreign snapshot horizon persisted" || probes != 1 {
			t.Fatal("capture fixture deadline expiry returned generic refusal")
		}
		if len(logs) != 1 || logs[0] != "capture fixture probe classes begin_failed=0 timeout=1 unhealthy=0 missing_account=0 safe=0 unsafe=0" {
			t.Fatal("capture fixture failure probe classes line must appear exactly once with exact counts")
		}
	})
}
