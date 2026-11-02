package localcell_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Blocking the copy's final write holds the small child payload outside the
// diagnostic buffer while the child exits. Cmd.Wait must wait for this drain; Process.Wait
// may reap the child but cannot certify that the diagnostic capture is complete.
type nativeServiceStderrDrainBarrier struct {
	target  io.Writer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *nativeServiceStderrDrainBarrier) Write(data []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.target.Write(data)
}

func TestNativeServiceForegroundCaptureDrainOrdering(t *testing.T) {
	const childArgument = "native-service-stderr-drain-fixture"
	const payload = "native service drain fixture: initial bytes\nnative service drain fixture: final bytes\n"
	if os.Args[len(os.Args)-1] == childArgument {
		_, _ = io.WriteString(os.Stderr, payload)
		os.Exit(23)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	// Under go test -cover (CI runs go-rest with -covermode=atomic) the test
	// binary is coverage-instrumented and go test sets GOCOVERDIR for it; a
	// re-executed child that does not inherit GOCOVERDIR appends "warning:
	// GOCOVERDIR not set, no coverage data emitted" to its stderr at exit. The
	// child environment allowlist forwards GOCOVERDIR; this private directory
	// keeps the child's counters out of the parent's profile and keeps the exact
	// payload assertion below valid under both builds.
	t.Setenv("GOCOVERDIR", t.TempDir())
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate stderr drain fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var logged strings.Builder
	diagnostics := nativeServiceFailureDiagnostic{
		goos: "windows", root: t.TempDir(), home: t.TempDir(),
		logf: func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
		run:  func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	diagnostics.enter("ready")
	barrier := &nativeServiceStderrDrainBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer release()
	command := exec.CommandContext(ctx, host, "-test.run=^TestNativeServiceForegroundCaptureDrainOrdering$", "--", childArgument)
	exited, err := diagnostics.startForeground(command, func(target io.Writer) io.Writer {
		barrier.target = target
		return barrier
	})
	if err != nil {
		t.Fatal("cannot start stderr drain fixture")
	}
	defer func() {
		release()
		cancel()
		if exited == nil {
			return
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("stderr drain fixture cleanup did not finish")
		}
	}()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("stderr drain fixture did not reach its explicit copy barrier")
	}
	if len(diagnostics.foregroundStderr.snapshot()) != 0 {
		t.Fatal("stderr drain fixture failed to hold the payload outside the capture")
	}
	// The child has no remaining work after its small pipe write. Keep the copy
	// blocked while independently observing completion; never receive cleanup's
	// channel here. Both Process.Wait mutants publish while this barrier is held.
	deadline := time.NewTimer(300 * time.Millisecond)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	defer deadline.Stop()
held:
	for {
		select {
		case <-deadline.C:
			break held
		case <-ticker.C:
			if diagnostics.foregroundExit.finished() {
				t.Fatal("foreground exit published before the explicit stderr drain barrier released the full payload")
			}
		case <-ctx.Done():
			t.Fatal("stderr drain fixture observation timed out")
		}
	}
	release()
	for !diagnostics.foregroundExit.finished() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("foreground completion was not published after stderr drained")
		}
	}
	if string(diagnostics.foregroundStderr.snapshot()) != payload {
		t.Fatal("published foreground exit lacked the complete final stderr payload")
	}
	var exitError *exec.ExitError
	if !errors.As(diagnostics.foregroundExit.exitError(), &exitError) || exitError.ExitCode() != 23 {
		t.Fatal("stderr drain fixture lost the typed process exit failure")
	}
	// Exercise the actual diagnostic observer before cleanup receives anything.
	// A future observer that consumes the receipt makes this cleanup assertion fail.
	diagnostics.logFailure(errors.New("fixture failure"), nil)
	if !strings.Contains(logged.String(), payload) {
		t.Fatal("diagnostic observation lost the fully drained stderr payload")
	}
	select {
	case receipt := <-exited:
		if !errors.As(receipt, &exitError) || exitError.ExitCode() != 23 {
			t.Fatal("diagnostic observation changed cleanup's typed exit receipt")
		}
		// The cleanup receipt has now been observed exactly once.
		exited = nil
	case <-ctx.Done():
		t.Fatal("diagnostic observation consumed cleanup's exit receipt")
	}
}
