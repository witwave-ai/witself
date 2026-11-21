package localcell

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// ObserveStartFailureForTest retains a returned child-launch error before step
// strips its cause. The original operations, arguments and results are unchanged.
func (c *Cell) ObserveStartFailureForTest() func(error) string {
	ops := c.principalStartOperations()
	launch := ops.launch
	var childErr error
	ops.launch = func(ctx context.Context, m *Manifest, component string, prepare func() error) error {
		childErr = launch(ctx, m, component, prepare)
		return childErr
	}
	c.installerStart = &ops
	return func(err error) string {
		if err != nil {
			message := err.Error()
			for _, step := range []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"} {
				prefix := "local cell start: " + step + " failed"
				if message == prefix || strings.HasPrefix(message, prefix+": ") {
					if childErr != nil && (step == "server" || step == "worker") {
						err = childErr
					}
					return launchFailureClass(step, err)
				}
			}
		}
		return launchFailureClass("start", err)
	}
}

const windowsLaunchTimeout = syscall.Errno(1460) // Win32 ERROR_TIMEOUT

func launchFailureClass(step string, err error) string {
	kind := "other"
	private := runtimeconfig.WindowsPrivateFailureClass(err)
	var readiness *readinessError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, os.ErrNotExist):
		kind = "not-found"
	case private == "sharing_violation" || (runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32))):
		kind = "sharing-violation"
	case private == "access_denied" || errors.Is(err, os.ErrPermission):
		kind = "access-denied"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT),
		errors.As(err, &timeout) && timeout.Timeout(), runtime.GOOS == "windows" && errors.Is(err, windowsLaunchTimeout),
		errors.As(err, &readiness) && readiness.deadlineExceeded,
		err != nil && err.Error() == "local cell PostgreSQL start deadline exceeded":
		kind = "timeout"
	}
	class := "step=" + step + " kind=" + kind
	if err != nil {
		switch err.Error() {
		case "cannot create suspended local cell component":
			return class + " at=create-process"
		case "cannot resume local cell component":
			return class + " at=resume-process"
		case "local cell PostgreSQL start deadline exceeded", "PostgreSQL exited before readiness":
			return class + " at=readiness"
		}
	}
	return class
}

func TestLaunchFailureClass(t *testing.T) {
	sharing, nativeTimeout := "other", "other"
	if runtime.GOOS == "windows" {
		sharing = "sharing-violation"
		nativeTimeout = "timeout"
	}
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{os.ErrNotExist, "not-found"}, {os.ErrPermission, "access-denied"},
		{syscall.Errno(32), sharing}, {context.DeadlineExceeded, "timeout"},
		{os.ErrDeadlineExceeded, "timeout"}, {syscall.EAGAIN, "timeout"}, {syscall.ETIMEDOUT, "timeout"},
		{windowsLaunchTimeout, nativeTimeout},
		{errors.New("unrecognized private error"), "other"},
	} {
		err := &os.PathError{Op: "open", Path: "private", Err: tc.err}
		if launchFailureClass("postgres", err) != "step=postgres kind="+tc.kind {
			t.Errorf("launch failure class differs for expected kind %s", tc.kind)
		}
	}
}

// ReadinessFailureForTest exposes the observation from before startup rollback
// to the external lifecycle proof tests, without adding a production API.
func ReadinessFailureForTest(err error) (component, cause string, state ProcessState, observationFailed, ok bool) {
	var failure *readinessError
	if !errors.As(err, &failure) {
		return "", "", ProcessState{}, false, false
	}
	return failure.component, failure.Error(), failure.state, failure.observationFailed, true
}

// ReadinessExitDiagnosticForTest shares the production value-free renderer with
// the external lifecycle diagnostic contract, without exporting a runtime API.
func ReadinessExitDiagnosticForTest(err error) string {
	var exit *readinessExitError
	if !errors.As(err, &exit) {
		return ""
	}
	return exit.observationDiagnostic()
}

func ReadinessExitForTest(waitCompleted, recoveredAlive bool) error {
	return &readinessExitError{
		observation: &readinessError{component: "server"}, component: "server",
		exitStatus: "unavailable", diagnostic: "value-free diagnostic unavailable",
		waitCompleted: waitCompleted, recoveredAlive: recoveredAlive,
	}
}
