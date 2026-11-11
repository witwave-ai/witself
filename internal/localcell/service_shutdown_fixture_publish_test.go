package localcell

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShutdownFixtureRetryPublish(t *testing.T) {
	for _, row := range []struct {
		name     string
		failures []error
	}{
		{"first-success", nil},
		{"sharing-backoff", []error{syscall.Errno(32), syscall.Errno(32), syscall.Errno(32), syscall.Errno(32), syscall.Errno(32), syscall.Errno(32)}},
		{"access-backoff", []error{syscall.Errno(5), syscall.Errno(5), syscall.Errno(5), syscall.Errno(5), syscall.Errno(5), syscall.Errno(5)}},
		{"mixed-wrapped", []error{fmt.Errorf("wrapped: %w", syscall.Errno(32)), fmt.Errorf("wrapped: %w", syscall.Errno(5))}},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := time.Unix(0, 0)
			var sleeps []time.Duration
			writes, classified := 0, 0
			attempts, err := shutdownFixtureRetryPublish(func() error {
				writes++
				if writes <= len(row.failures) {
					return row.failures[writes-1]
				}
				return nil
			}, func(err error) string {
				classified++
				if classified > len(row.failures) || err != row.failures[classified-1] {
					t.Fatal("classifier did not receive each failed attempt in order")
				}
				return shutdownFixtureReadErrorClass(err)
			}, func(d time.Duration) {
				sleeps = append(sleeps, d)
				clock = clock.Add(d)
			}, func() time.Time { return clock })
			want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 16 * time.Millisecond}
			if err != nil || attempts != len(row.failures)+1 || writes != attempts || classified != len(row.failures) {
				t.Fatal("publication success lost its attempt count or per-failure classification")
			}
			if !slices.Equal(sleeps, want[:len(row.failures)]) {
				t.Fatal("publication retry lost the 1, 2, 4, 8, 16, 16 millisecond backoff")
			}
		})
	}
}

func TestShutdownFixtureRetryPublishNonTransient(t *testing.T) {
	for _, row := range []struct {
		name  string
		err   error
		class string
	}{
		{"not-exist", os.ErrNotExist, "not_exist"},
		{"validation", errors.New("fixture validation refusal"), "validation"},
		{"other", errors.New("fixture other refusal"), "other"},
		{"unrecognized", errors.New("fixture unknown refusal"), "unrecognized"},
	} {
		t.Run(row.name, func(t *testing.T) {
			writes, classified := 0, 0
			attempts, err := shutdownFixtureRetryPublish(func() error {
				writes++
				if writes > 1 {
					t.Fatal("non-transient publication was retried")
				}
				return row.err
			}, func(err error) string {
				classified++
				if err != row.err {
					t.Fatal("classifier lost the original non-transient error")
				}
				return row.class
			}, func(time.Duration) { t.Fatal("non-transient publication slept") }, func() time.Time { return time.Unix(0, 0) })
			if attempts != 1 || writes != 1 || classified != 1 || err != row.err {
				t.Fatal("non-transient publication lost its first error or attempt count")
			}
		})
	}
}

func TestShutdownFixtureRetryPublishExhaustion(t *testing.T) {
	for _, class := range []string{"sharing_violation", "access_denied"} {
		t.Run(class, func(t *testing.T) {
			clock := time.Unix(0, 0)
			start := clock
			var sleeps []time.Duration
			var last error
			writes, classified := 0, 0
			attempts, err := shutdownFixtureRetryPublish(func() error {
				writes++
				// Independent of the implementation's budget and clock checks:
				// a missing deadline must fail promptly, not hang the test.
				if writes > 140 {
					t.Fatal("publication retry exceeded bounded attempt guard; budget check missing")
				}
				last = fmt.Errorf("fixture transient attempt %d", writes)
				return last
			}, func(err error) string {
				classified++
				if err != last {
					t.Fatal("classifier did not receive the current transient error")
				}
				return class
			}, func(d time.Duration) {
				if d < time.Millisecond || d > 16*time.Millisecond {
					t.Fatal("publication retry sleep exceeded its backoff bounds")
				}
				sleeps = append(sleeps, d)
				clock = clock.Add(d)
			}, func() time.Time { return clock })
			if err == nil || err != last {
				t.Fatal("publication exhaustion must return the last transient error, never nil")
			}
			if attempts != writes || classified != attempts || len(sleeps) != attempts-1 {
				t.Fatal("publication exhaustion lost its attempt count, sleeps or per-failure classification")
			}
			elapsed := clock.Sub(start)
			if elapsed < shutdownFixturePublishBudget || elapsed > shutdownFixturePublishBudget+16*time.Millisecond {
				t.Fatal("publication exhaustion did not honor the budget within one capped backoff")
			}
		})
	}
}

func TestWindowsShutdownFixturePreforceMarkerSourceContract(t *testing.T) {
	source := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "TestWindowsShutdownEscalation")
	_, preforce, found := strings.Cut(source, `t.Run("preforce-creation-mismatch"`)
	preforce, _, bounded := strings.Cut(preforce, "t.Run(")
	wait := `waitShutdownMarker(t, f.cell.path("tmp", "cancel-requested"), shutdownFixtureLivenessBudget(f.startup))`
	check := `if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); err != nil {`
	if !found || !bounded || strings.Count(preforce, wait) != 1 || strings.Index(preforce, check) <= strings.Index(preforce, wait) ||
		!strings.Contains(preforce, `t.Error("force stage reached without actual accepted cancellation")`) {
		t.Fatal("preforce callback must wait for marker publication before its positive assertion")
	}
}
