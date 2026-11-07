package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestServiceFailureReceiptClientCancellationPreservesOtherFailures(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			}
			cancel()
			ctx = context.WithValue(ctx, serviceControlCommandKey{}, true)
			abandoned := serviceControlCancellation(ctx)
			var located *serviceFailure
			if !errors.Is(abandoned, ctx.Err()) || !errors.As(abandoned, &located) || located.site != "control_cancelled" {
				t.Fatal("client cancellation lost its cause or site")
			}
			failure := serviceFailureAt(errors.New("synthetic failure"), "supervisor_stop")
			var out bytes.Buffer
			joined := fmt.Errorf("command: %w", errors.Join(abandoned, failure))
			WriteServiceHostFailure(&out, joined, "supervisor_command")
			WriteServiceHostFailure(&out, joined, "supervisor_command")
			if out.String() != "native service host site=supervisor_stop\n" {
				t.Fatal("client cancellation hid an independent failure or emitted a host failure receipt")
			}
			// Cancellation of the host itself keeps the existing site receipt.
			hostCtx := context.WithValue(ctx, serviceControlHostContextKey{}, ctx)
			out.Reset()
			WriteServiceHostFailure(&out, serviceControlCancellation(hostCtx), "supervisor_command")
			if out.String() != "native service host site=control_cancelled\n" {
				t.Fatal("host cancellation lost its failure receipt")
			}
		})
	}
}

// Reproduce the real propagation through call, command and Run. The first stop
// is startup and the second is the sole failed authenticated control operation.
// Windows must latch that refusal instead of retrying it during host shutdown.
// No native service manager or child is started.
func TestServiceFailureReceiptOnceActualControlPath(t *testing.T) {
	testServiceFailureReceiptActualControlPath(t, runtime.GOOS == "windows")
}

// Execute the Windows refusal policy through the real local control transport
// on every platform, so a Linux run also covers its deferred-shutdown boundary.
func TestServiceFailureReceiptOnceWindowsPolicyActualControlPath(t *testing.T) {
	testServiceFailureReceiptActualControlPath(t, true)
}

func testServiceFailureReceiptActualControlPath(t *testing.T, windowsPolicy bool) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	root := filepath.Join(t.TempDir(), "cell")
	if runtime.GOOS != "windows" {
		var err error
		root, err = os.MkdirTemp("/tmp", "lc-receipt-")
		if err != nil {
			t.Fatal("cannot create failure receipt fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	if privateDir(root, true) != nil {
		t.Fatal("cannot secure failure receipt fixture")
	}
	cell, err := New(Config{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal("cannot configure failure receipt fixture")
	}
	if cell.layout() != nil {
		t.Fatal("cannot prepare failure receipt fixture layout")
	}
	manifest := &Manifest{
		LayoutVersion: LayoutVersion, InstallationID: "lc_test", CreatedAt: time.Now().UTC(),
		Database: localDatabaseName, Role: "localcell", Installed: true,
		Ports: Ports{Postgres: 21101, ServerAPI: 21102, ServerHealth: 21103, ServerMetrics: 21104, WorkerHealth: 21105, WorkerMetrics: 21106},
	}
	if writeAtomic(cell.path("manifest.json"), manifest, nil) != nil {
		t.Fatal("cannot write failure receipt fixture manifest")
	}
	host, err := os.Executable()
	if err != nil {
		t.Fatal("failure receipt fixture executable unavailable")
	}
	if cell.PrepareService(t.Context(), ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", cell.cfg.Root}}}) != nil {
		t.Fatal("cannot prepare failure receipt service")
	}
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	hooks.windowsShutdownPolicy = windowsPolicy
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
	if hostErr != nil {
		t.Fatal("failure receipt host did not exit cleanly")
	}
	if controlErr == nil {
		t.Fatal("failed control stop was not rejected")
	}
	wantStops := 3
	if windowsPolicy {
		wantStops = 2
	}
	if stops != wantStops {
		t.Fatal("failed control stop did not preserve shutdown policy")
	}
	descriptor, err := cell.ServiceDescriptor()
	if err != nil {
		t.Fatal("cannot read failure receipt service descriptor")
	}
	if descriptor.Faulted != windowsPolicy || descriptor.DesiredState != "stopped" {
		t.Fatal("failed control stop did not preserve durable fault and stopped intent")
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal("cannot read receipt")
	}
	count := strings.Count(string(data), "native service host site=")
	if count != 1 {
		t.Fatal("one failed control operation did not emit exactly one site receipt")
	}
	if strings.Count(string(data), "native service host site=supervisor_stop\n") != 1 {
		t.Fatal("failed control operation lost its inner supervisor_stop site")
	}
}
