package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServiceFailureReceiptPropagation(t *testing.T) {
	for _, shape := range []string{"inner_only", "outer_only", "both", "joined", "wrapped_join", "outer_attachment"} {
		t.Run(shape, func(t *testing.T) {
			cause := errors.New("private Windows filesystem object required")
			inner := serviceFailureAt(cause, "supervisor_stop")
			var out bytes.Buffer
			want := "supervisor_stop"
			switch shape {
			case "inner_only": // Backoff or HTTP translation swallows the error.
				WriteServiceHostFailure(&out, inner, "unknown")
			case "outer_only":
				WriteServiceHostFailure(&out, serviceFailureAt(cause, "supervisor_command"), "unknown")
				want = "supervisor_command"
			case "both":
				WriteServiceHostFailure(&out, inner, "unknown")
				WriteServiceHostFailure(&out, serviceFailureFallback(inner, "supervisor_command"), "host_config")
			case "joined":
				WriteServiceHostFailure(&out, inner, "unknown")
				WriteServiceHostFailure(&out, errors.Join(inner, inner), "host_config")
			case "wrapped_join":
				joined := fmt.Errorf("host: %w", errors.Join(inner, inner))
				WriteServiceHostFailure(&out, joined, "host_config")
				WriteServiceHostFailure(&out, joined, "host_config")
			case "outer_attachment":
				outer := serviceFailureAt(inner, "startup_stop")
				WriteServiceHostFailure(&out, outer, "host_config")
				WriteServiceHostFailure(&out, inner, "unknown")
			}
			if strings.Count(out.String(), "native service host site=") != 1 {
				t.Fatalf("failure path receipt count = %d, want 1", strings.Count(out.String(), "native service host site="))
			}
			if out.String() != "native service host site="+want+"\n" {
				t.Fatal("failure path lost its most precise site")
			}
		})
	}
}

func TestServiceFailureReceiptJoinedIndependentFailures(t *testing.T) {
	cause := errors.New("private Windows filesystem object required")
	first := serviceFailureAt(cause, "supervisor_stop")
	second := serviceFailureAt(cause, "supervisor_stop")
	var out bytes.Buffer
	WriteServiceHostFailure(&out, first, "unknown")
	WriteServiceHostFailure(&out, fmt.Errorf("host: %w", errors.Join(first, second)), "host_config")
	WriteServiceHostFailure(&out, second, "unknown")
	if out.String() != strings.Repeat("native service host site=supervisor_stop\n", 2) {
		t.Fatal("independent failures sharing a site did not retain one receipt each")
	}
}

// Reproduce the real propagation through call, command and Run. The first stop
// is startup, the second is the sole failed authenticated control operation, and
// the third is clean shutdown. No native service manager or child is started.
func TestServiceFailureReceiptOnceActualControlPath(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	cell, manifest := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-receipt-")
		if err != nil {
			t.Fatal("cannot create short private fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		cell, err = New(Config{Root: root, Timeout: time.Second})
		if err != nil || cell.layout() != nil || writeAtomic(cell.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare short private fixture")
		}
	}
	prepareSupervisorTest(t, cell)
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	hooks.ticks = make(chan time.Time)
	ready := make(chan struct{})
	hooks.health = func(context.Context) error { close(ready); return nil }
	stops := 0
	hooks.stop = func(context.Context) error {
		stops++
		if stops == 2 {
			return errors.New("private Windows filesystem object required")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stderr, err := os.CreateTemp(t.TempDir(), "receipt")
	if err != nil {
		t.Fatal("cannot capture receipt")
	}
	previous := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = previous; _ = stderr.Close() }()
	done := make(chan struct{})
	var hostErr error
	go func() {
		hostErr = cell.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: time.Second})
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("foreground fixture cleanup did not finish")
		}
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatal("foreground host exited before control request")
	case <-ctx.Done():
		t.Fatal("foreground never became ready")
	}
	_, controlErr := cell.Control(ctx, "stop")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground did not stop")
	}
	if hostErr != nil || controlErr == nil || stops != 3 {
		t.Fatal("fixture failed to exercise one rejected stop and clean startup/shutdown")
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal("cannot read receipt")
	}
	count := strings.Count(string(data), "native service host site=")
	if count != 1 {
		t.Fatalf("one failed control operation emitted %d site receipts, want 1", count)
	}
	if strings.Count(string(data), "native service host site=supervisor_stop\n") != 1 {
		t.Fatal("failed control operation lost its inner supervisor_stop site")
	}
}
