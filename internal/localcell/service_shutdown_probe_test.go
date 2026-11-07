package localcell

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type shutdownProbeReadObservation struct {
	phase   string
	cause   string
	retries int
}

var errShutdownProbeResult = errors.New("no-console probe completed unsuccessfully")

// Readiness means a complete validated read, not merely a visible pathname.
// The caller owns the one deadline; neither retries nor successful publication
// create another allowance. Keep this helper in the test fixture only.
func readShutdownProbeResult(ctx context.Context, read func() ([]byte, error), classify func(error) string, wait func(context.Context) error) ([]byte, shutdownProbeReadObservation, error) {
	observation := shutdownProbeReadObservation{phase: "private_read", cause: "none"}
	for {
		if err := ctx.Err(); err != nil {
			return nil, observation, err
		}
		data, err := read()
		if ctx.Err() != nil {
			return nil, observation, ctx.Err()
		}
		if err == nil {
			observation.phase = "complete"
			switch string(data) {
			case "0":
				observation.cause = "none"
				return data, observation, nil
			case "71", "72", "73", "74":
				observation.cause = "non_success"
			default:
				observation.cause = "malformed"
			}
			return data, observation, errShutdownProbeResult
		}
		observation.cause = shutdownProbeReadClass(classify(err))
		if observation.cause != "missing" && observation.cause != "sharing" {
			return nil, observation, err
		}
		observation.retries++
		if err := wait(ctx); err != nil {
			return nil, observation, err
		}
	}
}

func shutdownProbeReadClass(class string) string {
	switch class {
	case "missing", "sharing", "privacy", "access", "io":
		return class
	default:
		return "other"
	}
}

func TestShutdownProbeValidatedReadTransients(t *testing.T) {
	sharing := errors.New("synthetic publication sharing")
	reads, waits := 0, 0
	want := []byte("0")
	ctx := &shutdownProbeClockContext{Context: t.Context(), deadline: time.Unix(10, 0), done: make(chan struct{})}
	deadline, _ := ctx.Deadline()
	data, observation, err := readShutdownProbeResult(ctx, func() ([]byte, error) {
		reads++
		switch reads {
		case 1:
			return nil, os.ErrNotExist
		case 2:
			return nil, sharing
		default:
			return want, nil
		}
	}, func(err error) string {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}
		if errors.Is(err, sharing) {
			return "sharing"
		}
		return "other"
	}, func(got context.Context) error {
		waits++
		gotDeadline, _ := got.Deadline()
		if got != ctx || !gotDeadline.Equal(deadline) {
			t.Fatal("probe retry replaced the original deadline")
		}
		return nil
	})
	if err != nil || string(data) != "0" || &data[0] != &want[0] {
		t.Fatal("probe did not return the successful validated read")
	}
	if reads != 3 || waits != 2 || observation.retries != 2 || observation.phase != "complete" || observation.cause != "none" {
		t.Fatal("probe transient read sequence diverged")
	}
}

func TestShutdownProbeValidatedReadPermanentRefusal(t *testing.T) {
	for _, class := range []string{"privacy", "access", "io", "other", "unrecognized-private-value"} {
		t.Run(class, func(t *testing.T) {
			refusal := errors.New("synthetic permanent refusal")
			reads, waits := 0, 0
			data, observation, err := readShutdownProbeResult(t.Context(), func() ([]byte, error) {
				reads++
				return nil, refusal
			}, func(error) string { return class }, func(context.Context) error {
				waits++
				return context.Canceled
			})
			wantClass := class
			if class == "unrecognized-private-value" {
				wantClass = "other"
			}
			if !errors.Is(err, refusal) || len(data) != 0 || reads != 1 || waits != 0 || observation.retries != 0 || observation.phase != "private_read" || observation.cause != wantClass {
				t.Fatal("probe permanent refusal was retried or disclosed an unrecognized class")
			}
		})
	}
}

func TestShutdownProbeValidatedReadCompletedFailure(t *testing.T) {
	for _, value := range []string{"71", "72", "73", "74", "", "0\n", "unexpected"} {
		reads, waits := 0, 0
		data, observation, err := readShutdownProbeResult(t.Context(), func() ([]byte, error) {
			reads++
			return []byte(value), nil
		}, func(error) string { return "other" }, func(context.Context) error {
			waits++
			return context.Canceled
		})
		wantClass := "malformed"
		if value == "71" || value == "72" || value == "73" || value == "74" {
			wantClass = "non_success"
		}
		if !errors.Is(err, errShutdownProbeResult) || string(data) != value || reads != 1 || waits != 0 || observation.retries != 0 || observation.phase != "complete" || observation.cause != wantClass {
			t.Fatal("completed probe failure was retried or accepted")
		}
	}
}

// An injected clock advances only when the poll waits. No wall-clock timeout
// can accidentally kill a retry mutant or hide a renewed deadline.
type shutdownProbeClockContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	err      error
}

func (c *shutdownProbeClockContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *shutdownProbeClockContext) Done() <-chan struct{}       { return c.done }
func (c *shutdownProbeClockContext) Err() error                  { return c.err }

func TestShutdownProbeValidatedReadCancellation(t *testing.T) {
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, phase := range []string{"before_read", "during_read", "wait"} {
			ctx := &shutdownProbeClockContext{Context: t.Context(), deadline: time.Unix(10, 0), done: make(chan struct{})}
			complete := func() { ctx.err = terminal; close(ctx.done) }
			if phase == "before_read" {
				complete()
			}
			reads, waits := 0, 0
			data, observation, err := readShutdownProbeResult(ctx, func() ([]byte, error) {
				reads++
				if phase == "during_read" {
					complete()
					return []byte("0"), nil
				}
				return nil, os.ErrNotExist
			}, func(error) string { return "missing" }, func(got context.Context) error {
				waits++
				deadline, ok := got.Deadline()
				if got != ctx || !ok || !deadline.Equal(ctx.deadline) {
					t.Fatal("probe exhaustion reset its caller deadline")
				}
				complete()
				return terminal
			})
			wantReads, wantWaits := 1, 0
			switch phase {
			case "before_read":
				wantReads = 0
			case "wait":
				wantWaits = 1
			}
			if !errors.Is(err, terminal) || len(data) != 0 || reads != wantReads || waits != wantWaits || observation.retries != wantWaits {
				t.Fatal("probe lost cancellation or read beyond its original deadline")
			}
			if phase == "wait" && (observation.phase != "private_read" || observation.cause != "missing") {
				t.Fatal("probe exhaustion lost the last safe read class")
			}
		}
	}
}
