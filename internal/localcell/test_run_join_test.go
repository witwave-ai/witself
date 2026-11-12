package localcell

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type testRunReporter interface {
	Cleanup(func())
	Error(...any)
}

// StartJoinedTestRunForTest keeps the result separate from completion: a successful
// assertion may receive the result before cleanup still needs to join the run.
func StartJoinedTestRunForTest(t testRunReporter, stop func(), bound time.Duration, run func() error) <-chan error {
	result := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		result <- run()
	}()
	t.Cleanup(func() {
		stop()
		timer := time.NewTimer(bound)
		defer timer.Stop()
		select {
		case <-exited:
		case <-timer.C:
			t.Error("test worker did not stop before cleanup deadline")
		}
	})
	return result
}

type recordingTestRunReporter struct {
	cleanups []func()
	errors   []string
}

func (r *recordingTestRunReporter) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *recordingTestRunReporter) Error(args ...any) {
	r.errors = append(r.errors, fmt.Sprint(args...))
}

func TestJoinedTestRunCleanup(t *testing.T) {
	for _, mode := range []string{"returned", "body_consumed", "cancelled", "never_returns"} {
		t.Run(mode, func(t *testing.T) {
			var reporter recordingTestRunReporter
			stopped, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var stopOnce, releaseOnce sync.Once
			stop := func() { stopOnce.Do(func() { close(stopped) }) }
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer stop()
			defer unblock()
			want := errors.New("synthetic worker result")
			bound := serviceControlBudget(time.Second, 2)
			if mode == "never_returns" {
				bound = time.Millisecond
			}
			result := StartJoinedTestRunForTest(&reporter, stop, bound, func() error {
				defer close(returned)
				switch mode {
				case "cancelled":
					<-stopped
				case "never_returns":
					<-release
				}
				return want
			})
			defer func() { stop(); unblock(); <-returned }()
			if len(reporter.cleanups) != 1 {
				t.Fatal("worker cleanup was not registered")
			}
			if mode == "returned" {
				<-returned
			}
			if mode == "body_consumed" && <-result != want {
				t.Fatal("body lost worker result")
			}
			cleaned := make(chan struct{})
			go func() { defer close(cleaned); reporter.cleanups[0]() }()
			select {
			case <-cleaned:
			case <-time.After(serviceControlBudget(time.Second, 4)):
				// Release the artificial stuck worker and join the mutated cleanup
				// before failing, so an unbounded-join mutant cannot hang the suite.
				unblock()
				<-cleaned
				t.Fatal("worker cleanup exceeded its bound")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("worker cleanup did not stop the worker")
			}
			if mode == "never_returns" {
				if len(reporter.errors) != 1 || reporter.errors[0] != "test worker did not stop before cleanup deadline" {
					t.Fatal("worker cleanup lost its fixed timeout report")
				}
				unblock()
			} else if len(reporter.errors) != 0 {
				t.Fatal("completed worker cleanup reported an error")
			}
			if mode != "body_consumed" && <-result != want {
				t.Fatal("cleanup consumed or changed worker result")
			}
		})
	}
}
