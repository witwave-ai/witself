//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// Keep the same named total as localcell windowsSyntheticTestBudget. Test-only
// constants are package-local: 22 localcell tests reserve budget/33 each (60s in
// all) and the child-client matrix reserves 6s per row (12s); this separate
// harness package reserves 24s, excluding its bounded compilation.
const windowsSyntheticTestBudget = 90 * time.Second

// Exercise the production main/run dispatch and named-pipe transport in fresh
// processes. A stopped private installation needs no PostgreSQL or task adapter.
// GenerateConsoleCtrlEvent function and Process Creation Flags supply only the
// existing shared-console CTRL_BREAK shutdown contract.
func TestWindowsServiceForegroundSubprocessCommands(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	host := buildWindowsForegroundHarness(t)
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), windowsSyntheticTestBudget*4/15)
	t.Cleanup(cancel)
	t.Cleanup(func() { t.Logf("windows_synthetic elapsed_seconds=%.3f", time.Since(started).Seconds()) })
	// Reserve each command's pipe-copy WaitDelay inside the 24s runtime share.
	deadline, _ := ctx.Deadline()
	commandCtx, commandCancel := context.WithDeadline(ctx, deadline.Add(-windowsSyntheticTestBudget/90))
	t.Cleanup(commandCancel)
	root := privateHarnessRoot(t)
	for _, name := range []string{"spool", "run", "journal", "logs", "tmp", "home"} {
		if runtimeconfig.WindowsPrivateMkdir(filepath.Join(root, name)) != nil {
			t.Fatal("cannot create synthetic installation layout")
		}
	}
	write := func(path string, value any) {
		t.Helper()
		file, err := runtimeconfig.WindowsPrivateOpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
		if err != nil {
			t.Fatal("cannot create synthetic private record")
		}
		encodeErr := json.NewEncoder(file).Encode(value)
		closeErr := file.Close()
		if encodeErr != nil || closeErr != nil {
			t.Fatal("cannot publish synthetic private record")
		}
	}
	manifest := localcell.Manifest{LayoutVersion: localcell.LayoutVersion, InstallationID: "lc_windows_harness_fixture", CreatedAt: time.Now().UTC(), Database: "witself", Role: "localcell", Installed: true, Ports: localcell.Ports{Postgres: 21101, ServerAPI: 21102, ServerHealth: 21103, ServerMetrics: 21104, WorkerHealth: 21105, WorkerMetrics: 21106}}
	write(filepath.Join(root, "manifest.json"), manifest)
	cell, err := localcell.New(localcell.Config{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal("cannot open synthetic installation")
	}
	args := serviceFlags{root: root, timeout: time.Second}.hostArgs()
	descriptor := localcell.ServiceDescriptor{Adapter: "foreground", DesiredState: "stopped", Payload: localcell.ServicePayload{Host: host, Args: args}}
	if cell.PrepareService(ctx, descriptor) != nil {
		t.Fatal("cannot prepare synthetic foreground service")
	}
	environment := windowsForegroundHarnessEnvironment(t)
	allowance := 5 * time.Second
	invoke := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(commandCtx, allowance)
		defer cancel()
		command := exec.CommandContext(ctx, host, args...)
		command.Env = environment
		command.WaitDelay = windowsSyntheticTestBudget / 90
		return command.CombinedOutput()
	}
	baseline := time.Now()
	output, err := invoke("service", "install", "--root", root)
	allowance = max(5*time.Second, 10*time.Since(baseline))
	if err == nil || !strings.Contains(string(output), "service_manager_unavailable") {
		t.Fatal("Windows service install did not preserve adapter refusal")
	}
	var oldStart, oldBoot string
	for iteration := 0; iteration < 2; iteration++ {
		command := exec.CommandContext(commandCtx, host, "run", "--root", root)
		command.Env = environment
		command.WaitDelay = windowsSyntheticTestBudget / 90
		command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		var captured bytes.Buffer
		command.Stdout, command.Stderr = &captured, &captured
		if command.Start() != nil {
			t.Fatal("cannot start synthetic foreground host")
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		finished := false
		t.Cleanup(func() {
			if !finished {
				_ = command.Process.Kill()
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("foreground cleanup exceeded synthetic budget")
				}
			}
		})
		deadline := time.NewTimer(allowance)
		ticker := time.NewTicker(20 * time.Millisecond)
		ready := false
		for !ready {
			probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
			status, err := cell.Control(probeCtx, "status")
			probeCancel()
			if err == nil && status.State == "stopped" {
				ready = true
				break
			}
			select {
			case exitErr := <-done:
				finished = true
				windowsLogForegroundExit(t, command, captured.Bytes())
				switch classifyForegroundExit(commandCtx.Err(), ctx.Err(), exitErr) {
				case foregroundCommandBudgetExpired:
					if iteration == 0 {
						t.Fatal("foreground command budget expired before authenticated status on first start")
					}
					t.Fatal("foreground command budget expired before authenticated status after restart")
				case foregroundParentBudgetExpired:
					if iteration == 0 {
						t.Fatal("foreground synthetic budget expired before authenticated status on first start")
					}
					t.Fatal("foreground synthetic budget expired before authenticated status after restart")
				case foregroundExitedWithError:
					if iteration == 0 {
						t.Fatal("foreground host exited with error before authenticated status on first start")
					}
					t.Fatal("foreground host exited with error before authenticated status after restart")
				case foregroundExitedCleanly:
					if iteration == 0 {
						t.Fatal("foreground host exited cleanly before authenticated status on first start")
					}
					t.Fatal("foreground host exited cleanly before authenticated status after restart")
				}
			case <-ctx.Done():
				if iteration == 0 {
					t.Fatal("foreground readiness exceeded synthetic budget on first start")
				}
				t.Fatal("foreground readiness exceeded synthetic budget after restart")
			case <-deadline.C:
				if iteration == 0 {
					t.Fatal("foreground host did not expose authenticated status within allowance on first start")
				}
				t.Fatal("foreground host did not expose authenticated status within allowance after restart")
			case <-ticker.C:
			}
		}
		deadline.Stop()
		ticker.Stop()
		endpointData, err := os.ReadFile(filepath.Join(root, "service", "control-endpoint.json"))
		var endpoint struct {
			HostPID           int    `json:"host_pid"`
			HostStartIdentity string `json:"host_start_identity"`
			PipeName          string `json:"pipe_name"`
			BootID            string `json:"boot_id"`
		}
		if err != nil || json.Unmarshal(endpointData, &endpoint) != nil || endpoint.HostPID != command.Process.Pid || endpoint.HostStartIdentity == oldStart || endpoint.HostStartIdentity == "" || endpoint.PipeName == "" {
			t.Fatal("foreground endpoint lost fresh process binding")
		}
		if endpoint.BootID == "" || (oldBoot != "" && endpoint.BootID != oldBoot) {
			t.Fatal("foreground restart lost independent boot binding")
		}
		oldStart, oldBoot = endpoint.HostStartIdentity, endpoint.BootID
		for _, action := range []string{"status", "observe", "stop", "status", "observe"} {
			output, err := invoke("service", action, "--root", root)
			var status localcell.ServiceStatus
			if err != nil || json.Unmarshal(output, &status) != nil || status.DesiredState != "stopped" || status.State != "stopped" || status.Healthy || status.Generation == "" {
				t.Fatal("foreground command did not preserve durable stopped state")
			}
		}
		if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(command.Process.Pid)) != nil {
			t.Fatal("foreground CTRL_BREAK console dispatch unavailable")
		}
		select {
		case exitErr := <-done:
			finished = true
			switch classifyForegroundExit(commandCtx.Err(), ctx.Err(), exitErr) {
			case foregroundCommandBudgetExpired:
				windowsLogForegroundExit(t, command, captured.Bytes())
				if iteration == 0 {
					t.Fatal("foreground command budget expired during shutdown on first start")
				}
				t.Fatal("foreground command budget expired during shutdown after restart")
			case foregroundParentBudgetExpired:
				windowsLogForegroundExit(t, command, captured.Bytes())
				if iteration == 0 {
					t.Fatal("foreground synthetic budget expired during shutdown on first start")
				}
				t.Fatal("foreground synthetic budget expired during shutdown after restart")
			case foregroundExitedWithError:
				windowsLogForegroundExit(t, command, captured.Bytes())
				if iteration == 0 {
					t.Fatal("foreground host exited with error during shutdown on first start")
				}
				t.Fatal("foreground host exited with error during shutdown after restart")
			case foregroundExitedCleanly:
				// A clean exit is the expected shutdown result.
			}
		case <-ctx.Done():
			if iteration == 0 {
				t.Fatal("foreground shutdown exceeded synthetic budget on first start")
			}
			t.Fatal("foreground shutdown exceeded synthetic budget after restart")
		case <-time.After(allowance):
			if iteration == 0 {
				t.Fatal("foreground console shutdown exceeded allowance on first start")
			}
			t.Fatal("foreground console shutdown exceeded allowance after restart")
		}
		if _, err := os.Lstat(filepath.Join(root, "service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("foreground shutdown retained endpoint record")
		}
		pipeName, err := windows.UTF16PtrFromString(endpoint.PipeName)
		if err != nil {
			t.Fatal("invalid synthetic pipe name")
		}
		handle, err := windows.CreateFile(pipeName, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			_ = windows.CloseHandle(handle)
			t.Fatal("foreground shutdown retained a pipe instance")
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatal("foreground closed pipe could not be classified absent")
		}
		file, err := runtimeconfig.WindowsPrivateOpenFile(filepath.Join(root, ".service-host.lock"), os.O_RDWR)
		if err != nil {
			t.Fatal("cannot open stable foreground lease")
		}
		var overlapped windows.Overlapped
		lockErr := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if lockErr == nil {
			_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
		}
		_ = file.Close()
		if lockErr != nil {
			t.Fatal("foreground shutdown retained its lease")
		}
		receipt := captured.Bytes()
		if len(foregroundOutputLines(receipt)) > 1 {
			windowsLogForegroundExit(t, command, receipt)
		}
		if failure := foregroundShutdownFailure(receipt); failure != "" {
			t.Fatal(failure)
		}
	}
}

// Receipt keys emitted by localcell.(*Cell).Run in
// internal/localcell/service_supervisor.go. The portable source contract compares
// this closed allowlist against that emitter.
const windowsForegroundReceiptKeys = "phase|operation|site|journal_pending|journal_read_ok"

const windowsForegroundLeasePublishPattern = `native service host lease_publish class=(sharing_violation|access_denied|validation|other) attempts=[1-9][0-9]* elapsed_ms=[0-9]+`

func TestWindowsForegroundLeasePublishOutput(t *testing.T) {
	guard := regexp.MustCompile(`^` + windowsForegroundLeasePublishPattern + `$`)
	if guard.MatchString("native service host lease_publish class=other attempts=1 elapsed_ms=0 cause=private") {
		t.Fatal("Windows lease publication guard accepted an extra private field")
	}
}

// Only call after Wait has joined both output copiers. Each complete or partial
// line is independently guarded; rejected bytes are never logged or normalized
// into the vocabulary. Diagnostic visibility does not imply shutdown acceptance.
func windowsLogForegroundExit(t *testing.T, command *exec.Cmd, captured []byte) {
	t.Helper()
	t.Logf("foreground_output_lines=%d", len(foregroundOutputLines(captured)))
	for _, firstLine := range foregroundOutputLines(captured) {
		firstLine = strings.TrimSuffix(firstLine, "\r")
		if len(firstLine) <= 160 && regexp.MustCompile(`^(localcell: [a-z_]{1,48}|service_[a-z_]+( (`+windowsForegroundReceiptKeys+`)=[a-z_0-9]{1,24})*|service_control accept=rejected step=(peer:pid|peer:identity|peer:sid|duplicate|conn)|service_control action=stop response=unflushed|`+windowsForegroundLeasePublishPattern+`|native service host site=(host_config|host_descriptor|host_prepare|host_persisted_config|initial_descriptor|lease_directory|lease_record|lease_write|control_secret|control_transport|control_cancelled|endpoint_descriptor|endpoint_read|endpoint_write|endpoint_lease|endpoint_remove|supervisor_load|supervisor_traversal|supervisor_save|supervisor_start|supervisor_stop|supervisor_repair|supervisor_reconcile|supervisor_health|supervisor_command|health_lock|health_tree|health_database|health_credential|health_probe|health_ready|startup_reconcile|startup_repair|startup_stop|shutdown_repair|shutdown_stop|boot_binding|descriptor_write|unknown))$`).MatchString(firstLine) {
			t.Log(firstLine)
		}
	}
	t.Logf("exit_code=%d", command.ProcessState.ExitCode())
}

func windowsForegroundHarnessEnvironment(t *testing.T) []string {
	t.Helper()
	home, temporary := t.TempDir(), t.TempDir()
	result := []string{"WITSELF_RUNTIME=local", "WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(), "HOME=" + home, "USERPROFILE=" + home, "TEMP=" + temporary, "TMP=" + temporary}
	if value, ok := os.LookupEnv("SystemRoot"); ok {
		result = append(result, "SystemRoot="+value)
	}
	return result
}

func buildWindowsForegroundHarness(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal("cannot locate harness source")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("cannot locate Go tool")
	}
	query := exec.CommandContext(ctx, goTool, "env", "-json", "GOCACHE", "GOMODCACHE")
	query.WaitDelay = time.Second
	data, err := query.Output()
	var caches struct{ GOCACHE, GOMODCACHE string }
	if err != nil || json.Unmarshal(data, &caches) != nil || !filepath.IsAbs(caches.GOCACHE) || !filepath.IsAbs(caches.GOMODCACHE) {
		t.Fatal("cannot locate build caches")
	}
	host := filepath.Join(t.TempDir(), "localcellharness.exe")
	command := exec.CommandContext(ctx, goTool, "build", "-o", host, "./internal/tools/localcellharness")
	command.WaitDelay = time.Second
	command.Dir = source
	command.Env = append(windowsForegroundHarnessEnvironment(t), "PATH="+filepath.Dir(goTool), "GOCACHE="+caches.GOCACHE, "GOMODCACHE="+caches.GOMODCACHE, "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if err := command.Run(); err != nil {
		t.Fatal("cannot build synthetic foreground harness")
	}
	return host
}
