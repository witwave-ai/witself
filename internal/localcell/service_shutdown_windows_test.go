//go:build windows

package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

func TestWindowsShutdownChildHelper(_ *testing.T) {
	args := os.Args
	index := 0
	for index < len(args) && args[index] != "--" {
		index++
	}
	if index == len(args) {
		return
	}
	args = args[index+1:]
	if len(args) == 0 {
		os.Exit(70)
	}
	if args[0] == "probe" {
		// The probe is a detached child of the DETACHED_PROCESS fixture, so
		// ATTACH_PARENT_PROCESS always selects that fixture, never the runner.
		getConsole := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
		if window, _, _ := getConsole.Call(); window != 0 {
			os.Exit(71)
		}
		const attachParentProcess = ^uint32(0)
		result, _, callErr := attachConsoleProc.Call(uintptr(attachParentProcess))
		if result != 0 {
			_, _, _ = freeConsoleProc.Call()
			os.Exit(73)
		}
		if !errors.Is(callErr, windows.ERROR_INVALID_HANDLE) {
			os.Exit(74)
		}
		os.Exit(0)
	}
	if args[0] == "startup" {
		os.Exit(0)
	}
	mode := args[0]
	root, err := os.Getwd()
	if err != nil {
		os.Exit(76)
	}
	// GetConsoleWindow can also return zero for a hidden console. Every console
	// has at least one process, so require this console query to fail with the
	// exact no-console error before publishing the witness. The buffer is valid
	// even if more than one process is attached; any positive count fails.
	var consoleProcess uint32
	getConsoleProcesses := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	count, _, consoleErr := getConsoleProcesses.Call(uintptr(unsafe.Pointer(&consoleProcess)), 1)
	if count != 0 || !errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE) {
		os.Exit(77)
	}
	if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "console-absent"), []byte("absent")); err != nil {
		os.Exit(78)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var completedMu sync.Mutex
	completed := 0
	windowsShutdownHandlerCompleted = func(request serviceShutdownRequest) {
		if request.Action != "cancel" {
			return
		}
		completedMu.Lock()
		defer completedMu.Unlock()
		completed++
		// Retain a failed publication as a fixed process exit class. A second
		// file publication could fail for the same reason as the first one.
		if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "cancel-handled"), []byte(strconv.Itoa(completed))); err != nil {
			os.Exit(shutdownFixturePublishExitCode(err, false))
		}
	}
	var cancelCalls atomic.Uint32
	var cancelMu sync.Mutex
	requestCancel := func() {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "cancel-requested"), []byte(strconv.FormatUint(uint64(cancelCalls.Add(1)), 10))); err != nil {
			os.Exit(shutdownFixturePublishExitCode(err, true))
		}
		if mode != "ignore" && mode != "lost-force" && mode != "lost-all" && mode != "migration" {
			cancel()
		}
	}
	if mode == "lost" || mode == "lost-force" || mode == "lost-all" {
		lost := false
		windowsShutdownReply = func(r serviceShutdownRequest, _ serviceShutdownResponse) bool {
			if r.Action == "cancel" && (!lost || mode == "lost-all") {
				lost = true
				return false
			}
			return true
		}
	}
	config := runtimeconfig.LocalShutdown{Transport: runtimeconfig.LocalShutdownNamedPipe, DescriptorPath: os.Getenv("WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR")}
	channel, err := InstallLocalShutdown(ctx, requestCancel, config, "server")
	if err != nil {
		os.Exit(79)
	}
	if channel.WaitRelease(ctx) != nil {
		_ = channel.Close()
		os.Exit(0)
	}
	if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "store-entered"), []byte("entered")); err != nil {
		os.Exit(80)
	}
	if mode != "migration" {
		if channel.RuntimeReady() != nil {
			os.Exit(81)
		}
	}
	if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "runtime-started"), []byte("started")); err != nil {
		os.Exit(82)
	}
	if mode == "normal-probe" {
		// Run from the real shutdown fixture: its own DETACHED_PROCESS launch
		// is the parent whose console absence the detached probe observes.
		if len(args) != 2 {
			os.Exit(83)
		}
		probeBudget, err := time.ParseDuration(args[1])
		if err != nil || probeBudget <= 0 {
			os.Exit(83)
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, probeBudget)
		probe := exec.CommandContext(probeCtx, os.Args[0], "-test.run=^TestWindowsShutdownChildHelper$", "--", "probe")
		probe.Dir = root
		probe.Stdout, probe.Stderr = io.Discard, io.Discard
		probe.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		code := 0
		if err := probe.Run(); err != nil {
			code = 72
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
		}
		probeCancel()
		if _, err := shutdownFixturePublishMarker(filepath.Join(root, "tmp", "console-probe-result"), []byte(strconv.Itoa(code))); err != nil {
			os.Exit(75)
		}
	}
	<-ctx.Done()
	_ = channel.Close()
	os.Exit(0)
}

func shutdownFixturePublishMarker(path string, data []byte) (int, error) {
	return shutdownFixtureRetryPublish(func() error { return writeAtomicBytes(path, data, nil) },
		shutdownFixtureReadErrorClass, time.Sleep, time.Now)
}

type windowsShutdownFixture struct {
	cell    *Cell
	record  ProcessRecord
	handle  windows.Handle
	startup time.Duration
}

func (f windowsShutdownFixture) completionClass() string {
	handled, err := readPrivateFile(f.cell.path("tmp", "cancel-handled"))
	var code uint32
	exitErr := windows.GetExitCodeProcess(f.handle, &code)
	return shutdownFixtureCompletionClass(handled, err, code, exitErr)
}

// Replay the descriptor reader's checks separately so its fixed refusal does
// not hide which observation currently fails. This is a later snapshot, not a
// record of the original failure; a transient conflict may already be gone.
func shutdownFixtureReadDescriptor(root, path string) (serviceShutdownDescriptor, string, error) {
	if !shutdownDescriptorPath(root, path) {
		return serviceShutdownDescriptor{}, "path", shutdownRefused("shutdown:descriptor")
	}
	for _, check := range []struct {
		step string
		read func() error
	}{
		{"tree", func() error { return validatePrivateTree(root) }},
		{"root", func() error { return privateDir(root, false) }},
		{"service_dir", func() error { return privateDir(filepath.Join(root, "service"), false) }},
		{"children", func() error { return privateDir(filepath.Join(root, "service", "children"), false) }},
		{"record", func() error { return servicePrivateRecord(path) }},
	} {
		if err := check.read(); err != nil {
			return serviceShutdownDescriptor{}, check.step, err
		}
	}
	file, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return serviceShutdownDescriptor{}, "open", err
	}
	defer func() { _ = file.Close() }()
	d, err := decodeShutdownDescriptor(file)
	if err != nil {
		return d, "decode", err
	}
	return d, "none", nil
}

func (f windowsShutdownFixture) descriptorClass() string {
	d, step, readErr := shutdownFixtureReadDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
	rootErr := f.cell.validateRoot(false)
	service, serviceErr := f.cell.ServiceDescriptor()
	boot, bootErr := serviceBootID()
	match := shutdownFixtureDescriptorMatchClass(d, f.record, service, boot, readErr, serviceErr, bootErr)
	pending, scanErr := shutdownFixturePendingCount(f.cell.cfg.Root)
	return "descriptor=" + step + " error=" + shutdownFixtureObservationClass(readErr) +
		" root=" + shutdownFixtureObservationClass(rootErr) + " service=" + shutdownFixtureObservationClass(serviceErr) +
		" boot=" + shutdownFixtureObservationClass(bootErr) + " match=" + match +
		" pending=" + shutdownFixtureCountClass(pending) + " scan=" + shutdownFixtureObservationClass(scanErr)
}

// Measure this test binary's process startup before selecting behavioural waits.
func shutdownFixtureStartup(t *testing.T, c *Cell) time.Duration {
	t.Helper()
	// Subprocess hang guard only, not a behavioural latency assertion.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsShutdownChildHelper$", "--", "startup")
	var err error
	probe.Env, err = c.baseEnvironment()
	if err != nil {
		t.Fatal("startup fixture environment unavailable")
	}
	probe.Dir = c.cfg.Root
	probe.Stdout, probe.Stderr = io.Discard, io.Discard
	probe.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	started := time.Now()
	if probe.Run() != nil {
		t.Fatal("startup fixture did not complete")
	}
	return time.Since(started)
}

func launchShutdownFixture(t *testing.T, mode string, barrier bool) windowsShutdownFixture {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, _ := descriptorTestCell(t)
	startup := shutdownFixtureStartup(t, c)
	c.cfg.Timeout = max(20*startup, shutdownFixtureLivenessBudget(startup))
	environment, err := c.baseEnvironment()
	if err != nil {
		t.Fatal("fixture environment unavailable")
	}
	environment = appendEnvironment(environment, "WITSELF_RUNTIME=local", "WITSELF_HOME="+c.path("home"), "DSH_HOME="+c.path("home"))
	log, err := openPrivateFile(c.path("logs", "shutdown-fixture.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("fixture log unavailable")
	}
	defer func() { _ = log.Close() }()
	old := windowsLaunchCheckpoint
	var handle windows.Handle
	windowsLaunchCheckpoint = func(stage string, pi windows.ProcessInformation) error {
		if stage == "created" {
			handle = retainedLaunchHandle(t, pi.Process)
		}
		if barrier && stage == "transferred" {
			return errors.New("synthetic host exit before release")
		}
		return nil
	}
	defer func() { windowsLaunchCheckpoint = old }()
	args := []string{"-test.run=^TestWindowsShutdownChildHelper$", "--", mode}
	if mode == "normal-probe" {
		args = append(args, c.cfg.Timeout.String())
	}
	started := time.Now()
	err = c.launchWindows(os.Args[0], args, "server", environment, log, nil)
	if (err != nil) != barrier {
		t.Fatal("synthetic launch did not reach expected release fence")
	}
	var record ProcessRecord
	if readJSON(c.path("run", "server.json"), &record) != nil || record.LaunchKind != shutdownLaunchPipe {
		t.Fatal("synthetic pipe record unavailable")
	}
	if !barrier {
		waitShutdownMarker(t, c.path("tmp", "runtime-started"), max(10*time.Since(started), shutdownFixtureLivenessBudget(startup)))
	}
	f := windowsShutdownFixture{c, record, handle, time.Since(started)}
	// A fixture that exits on a failed marker publication can fail a subtest
	// that never reads the marker. Name the class there too.
	t.Cleanup(func() {
		var code uint32
		if t.Failed() && windows.GetExitCodeProcess(f.handle, &code) == nil && code >= 90 && code <= 97 {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
		}
	})
	return f
}

func waitShutdownMarker(t *testing.T, path string, budget time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	for {
		// Final-name visibility alone can precede the publisher's DELETE-handle
		// close. A successful private read also fences that rename handle.
		if _, err := readPrivateFile(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("synthetic shutdown marker unavailable")
		case <-time.After(time.Millisecond):
		}
	}
}

func (f windowsShutdownFixture) stop(t *testing.T) serviceStopOutcome {
	t.Helper()
	budget := shutdownFixtureBudget(f.startup)
	ctx, cancel := context.WithTimeout(t.Context(), max(10*f.startup+time.Second, 2*budget, shutdownFixtureLivenessBudget(f.startup)))
	defer cancel()
	return f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, budget)
}

func TestWindowsNoConsoleShutdown(t *testing.T) {
	originalLegacyAttempt, originalForceAttempt := windowsLegacyStopAttempt, windowsShutdownBeforeForce
	legacyAttempts, forceAttempts := 0, 0
	windowsLegacyStopAttempt = func() { legacyAttempts++ }
	windowsShutdownBeforeForce = func(*ProcessRecord) { forceAttempts++ }
	t.Cleanup(func() {
		windowsLegacyStopAttempt, windowsShutdownBeforeForce = originalLegacyAttempt, originalForceAttempt
	})
	// Establish that the entry witness is live on an explicitly recorded,
	// successful legacy launch before asserting its absence on refused paths.
	legacyCell, legacyRecord, _ := windowsControlFixture(t, false)
	legacyOutcome := legacyCell.stopChildOutcome(t.Context(), "server", ProcessState{PID: legacyRecord.PID, Alive: true, Matches: true})
	if !legacyOutcome.permitsDrain() || legacyAttempts != 1 || forceAttempts != 0 {
		t.Fatal("recorded legacy clean case did not exercise the console entry witness")
	}
	legacyAttempts = 0
	f := launchShutdownFixture(t, "normal-probe", false)
	if _, err := os.Stat(f.cell.path("tmp", "console-absent")); err != nil {
		t.Fatal("child did not prove absent console")
	}
	probeResult := f.cell.path("tmp", "console-probe-result")
	probeCtx, probeCancel := context.WithTimeout(t.Context(), max(f.cell.cfg.Timeout+10*f.startup, shutdownFixtureLivenessBudget(f.startup)))
	defer probeCancel()
	result, observation, err := readShutdownProbeResult(probeCtx,
		func() ([]byte, error) { return readPrivateFile(probeResult) },
		windowsShutdownProbeReadClass, func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
				return nil
			}
		})
	if err != nil {
		t.Logf("probe_result phase=%s cause=%s retries=%d", observation.phase, observation.cause, observation.retries)
		if observation.phase != "complete" {
			t.Fatal("no-console probe result unavailable")
		}
	}
	switch string(result) {
	case "0":
		// The probe witnessed the exact ERROR_INVALID_HANDLE from its parent.
	case "71":
		t.Fatal("detached probe unexpectedly had a console")
	case "72":
		t.Fatal("no-console probe launch failed")
	case "73":
		t.Fatal("detached probe unexpectedly attached to parent console")
	case "74":
		t.Fatal("detached AttachConsole probe did not prove ERROR_INVALID_HANDLE")
	default:
		t.Fatal("no-console probe exited unexpectedly")
	}
	bystander := launchShutdownFixture(t, "ignore", false)
	if outcome := f.stop(t); outcome.Kind != serviceStopGraceful {
		t.Fatal("console-free cancellation was not graceful")
	}
	if legacyAttempts != 0 || forceAttempts != 0 {
		t.Fatal("graceful pipe shutdown entered console or force control")
	}
	if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); err != nil {
		t.Fatal("real cancellation marker absent")
	}
	if _, alive, err := windowsProcessIdentity(bystander.handle); err != nil || !alive {
		t.Fatal("shutdown touched bystander")
	}
	descriptor, err := readShutdownDescriptor(bystander.cell.cfg.Root, bystander.record.ShutdownDescriptor)
	if err != nil {
		t.Fatal("pipe authentication refusal fixture unavailable")
	}
	descriptor.Nonce, err = shutdownRandom(16)
	if err != nil || writeAtomic(bystander.record.ShutdownDescriptor, descriptor, nil) != nil {
		t.Fatal("cannot prepare pipe authentication refusal")
	}
	if outcome := bystander.stop(t); outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:ack" {
		t.Fatal("pipe authentication refusal did not raise at ACK boundary")
	}
	if legacyAttempts != 0 || forceAttempts != 0 {
		t.Fatal("refused pipe request attempted console fallback or force")
	}
	if _, alive, err := windowsProcessIdentity(bystander.handle); err != nil || !alive {
		t.Fatal("pipe authentication refusal terminated child")
	}
	legacy := bystander.record
	legacy.LaunchKind = ""
	legacy.ShutdownEndpoint = ""
	legacy.ShutdownDescriptor = ""
	if writeAtomic(bystander.cell.path("run", "server.json"), legacy, nil) != nil {
		t.Fatal("cannot write legacy refusal fixture")
	}
	if outcome := bystander.stop(t); outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:pipe" {
		t.Fatal("unrecorded legacy launch did not refuse")
	}
	if legacyAttempts != 0 || forceAttempts != 0 {
		t.Fatal("unrecorded legacy launch attempted console or force control")
	}
	if _, alive, err := windowsProcessIdentity(bystander.handle); err != nil || !alive {
		t.Fatal("legacy refusal signaled child")
	}
}

func TestWindowsShutdownEscalation(t *testing.T) {
	t.Run("already-exited", func(t *testing.T) {
		f := launchShutdownFixture(t, "normal", false)
		d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
		if err != nil {
			t.Fatal("descriptor unavailable")
		}
		id, err := shutdownRandom(16)
		if err != nil {
			t.Fatal("request fixture unavailable")
		}
		if _, err := windowsShutdownRequest(t.Context(), d, d.request("cancel", id)); err != nil {
			t.Fatal("clean cancel request refused")
		}
		assertWindowsProcessExited(t, f.handle)
		state, err := f.cell.processState("server")
		if err != nil || state.Alive {
			t.Fatal("fixture did not expose exited process observation")
		}
		outcome := f.cell.stopChildOutcomeWithBudget(t.Context(), "server", state, f.startup)
		if outcome.Kind != serviceStopAlreadyExited {
			t.Fatal("verified exited incarnation did not classify already_exited")
		}
		if _, err := os.Stat(f.cell.path("run", "server.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("matching exited record retained")
		}
	})
	for _, mode := range []string{"ignore", "lost", "lost-force", "lost-all", "migration", "rights-absent"} {
		t.Run(mode, func(t *testing.T) {
			childMode := mode
			if mode == "rights-absent" {
				childMode = "ignore"
			}
			f := launchShutdownFixture(t, childMode, false)
			if mode == "rights-absent" {
				shutdownProcesses.Lock()
				entry := shutdownProcesses.entries[shutdownProcessKey(f.record)]
				entry.terminate = false
				shutdownProcesses.entries[shutdownProcessKey(f.record)] = entry
				shutdownProcesses.Unlock()
			}
			outcome := f.stop(t)
			switch mode {
			case "ignore", "lost-force":
				if outcome.Kind != serviceStopForced || outcome.err() != ErrServiceShutdownForced {
					t.Fatal("accepted ignored request was not classified forced")
				}
				assertWindowsProcessExited(t, f.handle)
			case "lost":
				if !outcome.permitsDrain() || outcome.Kind == serviceStopForced {
					reportLostACKOutcome(t, outcome)
					t.Logf("shutdown kind=%s detail=%s budget=%s startup=%s", outcome.Kind, outcome.Detail, shutdownFixtureBudget(f.startup), f.startup)
					t.Fatal("lost ACK with observed exit did not complete safely")
				}
			case "lost-all":
				if outcome.Kind != serviceStopUnknown || outcome.Detail != "shutdown:ack" {
					t.Fatal("lost ACK was treated as accepted cancellation")
				}
			case "migration":
				if outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:migration" {
					t.Fatal("migration force fence bypassed")
				}
			case "rights-absent":
				if outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:force_rights" {
					t.Fatal("missing retained terminate right bypassed")
				}
			}
			if mode == "migration" || mode == "rights-absent" || mode == "lost-all" {
				if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
					t.Fatal("refused escalation terminated child")
				}
			}
		})
	}
	t.Run("idempotent-ack", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
		if err != nil {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("descriptor unavailable")
		}
		id, err := shutdownRandom(16)
		if err != nil {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("request fixture unavailable")
		}
		request := d.request("cancel", id)
		first, err := windowsShutdownRequest(t.Context(), d, request)
		if err != nil {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("clean cancel request refused")
		}
		second, err := windowsShutdownRequest(t.Context(), d, request)
		if err != nil || first != second {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("retried cancel did not return identical ACK")
		}
		// The second ACK can arrive before its deferred callback. Observe handler
		// completion before reading the callback count, so a removed once guard fails.
		completion, cancel := context.WithTimeout(t.Context(), shutdownFixtureLivenessBudget(f.startup))
		defer cancel()
		for {
			handled, err := readPrivateFile(f.cell.path("tmp", "cancel-handled"))
			if err == nil && string(handled) == "2" {
				break
			}
			select {
			case <-completion.Done():
				t.Logf("handler completion budget=%s startup=%s", shutdownFixtureLivenessBudget(f.startup), f.startup)
				var code uint32
				exitErr := windows.GetExitCodeProcess(f.handle, &code)
				t.Logf("shutdown handler completion class: %s", shutdownFixtureCompletionClass(handled, err, code, exitErr))
				t.Fatal("second cancel handler did not complete")
			case <-time.After(max(f.startup/100, time.Millisecond)):
			}
		}
		data, err := readPrivateFile(f.cell.path("tmp", "cancel-requested"))
		if err != nil || string(data) != "1" {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("retry invoked cancellation more than once")
		}
		request.Action = "release"
		if _, err := windowsShutdownRequest(t.Context(), d, request); err == nil {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("request identifier reused for different action")
		}
	})
	t.Run("creation-mismatch", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
		if err != nil {
			t.Fatal("descriptor fixture unavailable")
		}
		f.record.StartTime += "-different"
		d.StartIdentity = f.record.StartTime
		if writeAtomic(f.record.ShutdownDescriptor, d, nil) != nil || writeAtomic(f.cell.path("run", "server.json"), f.record, nil) != nil {
			t.Fatal("cannot write creation mismatch fixture")
		}
		outcome := f.stop(t)
		if outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:incarnation" {
			t.Fatal("PID matched creation mismatch bypassed incarnation boundary")
		}
		if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
			t.Fatal("creation mismatch terminated bystander")
		}
		if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("creation mismatch reached downstream cancellation")
		}
	})
	t.Run("preforce-creation-mismatch", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		old := windowsShutdownBeforeForce
		t.Cleanup(func() { windowsShutdownBeforeForce = old })
		reached := false
		windowsShutdownBeforeForce = func(record *ProcessRecord) {
			reached = true
			// The ACK can precede the callback's marker publication, including
			// its sharing retry. Observe completion before the positive assertion.
			waitShutdownMarker(t, f.cell.path("tmp", "cancel-requested"), shutdownFixtureLivenessBudget(f.startup))
			if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); err != nil {
				t.Error("force stage reached without actual accepted cancellation")
			}
			record.StartTime += "-changed-after-ack"
		}
		outcome := f.stop(t)
		if !reached || outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:force" {
			t.Fatal("final force identity guard did not raise at escalation site")
		}
		if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
			t.Fatal("mismatched retained process was force terminated")
		}
	})
	t.Run("preforce-caller-cancel", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		old := windowsShutdownBeforeForce
		t.Cleanup(func() { windowsShutdownBeforeForce = old })
		reached := false
		windowsShutdownBeforeForce = func(_ *ProcessRecord) { reached = true; cancel() }
		outcome := f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, shutdownFixtureBudget(f.startup))
		if !reached || outcome.Kind != serviceStopUnknown || outcome.Detail != "shutdown:deadline" {
			t.Fatal("caller cancellation at final force fence was ignored")
		}
		if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
			t.Fatal("caller cancellation permitted automatic force")
		}
	})
}

func TestWindowsReleaseBarrier(t *testing.T) {
	f := launchShutdownFixture(t, "normal", true)
	if _, err := os.Stat(f.cell.path("tmp", "store-entered")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pre-release child entered store startup")
	}
	replacement, err := New(f.cell.cfg)
	if err != nil {
		t.Fatal("replacement host fixture unavailable")
	}
	f.cell = replacement
	if outcome := f.stop(t); !outcome.permitsDrain() || outcome.Kind == serviceStopForced {
		t.Fatal("replacement host could not cancel pre-release child")
	}
	if _, err := os.Stat(f.cell.path("tmp", "store-entered")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement host released stale child")
	}
	assertWindowsProcessExited(t, f.handle)
}

func TestWindowsChildDescriptorRefusals(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*serviceShutdownDescriptor)
	}{
		{"boot", func(d *serviceShutdownDescriptor) { d.BootID += "-wrong" }},
		{"creation", func(d *serviceShutdownDescriptor) { d.StartIdentity += "-wrong" }},
		{"generation", func(d *serviceShutdownDescriptor) { d.Generation += "-wrong" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			d, path, err := c.prepareShutdownLaunch("server")
			if err != nil {
				t.Fatal("descriptor preparation failed")
			}
			d.PID = os.Getpid()
			d.StartIdentity, _, err = processIdentity(os.Getpid())
			if err != nil {
				t.Fatal("self identity unavailable")
			}
			cfg := runtimeconfig.LocalShutdown{Transport: runtimeconfig.LocalShutdownNamedPipe, DescriptorPath: path}
			if writeAtomic(path, d, nil) != nil {
				t.Fatal("descriptor publication failed")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clean, err := InstallLocalShutdown(ctx, cancel, cfg, "server")
			if err != nil {
				t.Fatal("clean descriptor refused before mutation")
			}
			_ = clean.Close()
			row.change(&d)
			if writeAtomic(path, d, nil) != nil {
				t.Fatal("descriptor mutation failed")
			}
			bad, err := InstallLocalShutdown(ctx, cancel, cfg, "server")
			if bad != nil {
				_ = bad.Close()
			}
			if err == nil || err.Error() != "service_shutdown_refused: shutdown:descriptor" {
				t.Fatal("descriptor equality mutation bypassed raising site")
			}
		})
	}
	t.Run("nonce", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
		if err != nil {
			t.Fatal("descriptor unavailable")
		}
		d.Nonce, err = shutdownRandom(16)
		if err != nil || writeAtomic(f.record.ShutdownDescriptor, d, nil) != nil {
			t.Fatal("cannot mutate nonce fixture")
		}
		if outcome := f.stop(t); outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:ack" {
			t.Fatal("wrong nonce did not refuse authenticated request")
		}
		if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("wrong nonce reached cancellation")
		}
	})
	t.Run("foreign-server", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		original := windowsProcessTokenUser
		foreign, err := windows.StringToSid("S-1-5-21-100-200-300-9001")
		if err != nil {
			t.Fatal("foreign principal fixture unavailable")
		}
		windowsProcessTokenUser = func(windows.Handle) (*windows.SID, error) { return foreign, nil }
		t.Cleanup(func() { windowsProcessTokenUser = original })
		d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
		if err != nil {
			t.Fatal("descriptor unavailable")
		}
		id, err := shutdownRandom(16)
		if err != nil {
			t.Fatal("request fixture unavailable")
		}
		_, err = windowsShutdownRequest(t.Context(), d, d.request("cancel", id))
		if err == nil || err.Error() != "service_shutdown_refused: shutdown:pipe" {
			t.Fatal("foreign pipe server bypassed peer boundary")
		}
		if _, err := os.Stat(f.cell.path("tmp", "cancel-requested")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("foreign peer check leaked cancellation request")
		}
	})
	t.Run("tampered", func(t *testing.T) {
		f := launchShutdownFixture(t, "ignore", false)
		if writeAtomicBytes(f.record.ShutdownDescriptor, []byte("invalid"), nil) != nil {
			t.Fatal("cannot tamper descriptor fixture")
		}
		if outcome := f.stop(t); outcome.Kind != serviceStopRefused || outcome.Detail != "shutdown:descriptor" {
			t.Fatal("tampered descriptor was accepted")
		}
	})
}

func TestWindowsShutdownParentDeadline(t *testing.T) {
	f := launchShutdownFixture(t, "ignore", false)
	d, err := readShutdownDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)
	if err != nil {
		t.Fatal("deadline fixture descriptor unavailable")
	}
	id, err := shutdownRandom(16)
	if err != nil {
		t.Fatal("deadline fixture request unavailable")
	}
	// Establish accepted runtime-ready cancellation before measuring the parent
	// budget. A later identity or pipe refusal cannot masquerade as deadline proof.
	ack, err := windowsShutdownRequest(t.Context(), d, d.request("cancel", id))
	if err != nil || !ack.Accepted || ack.Phase != shutdownPhaseReady {
		t.Fatal("deadline fixture never accepted runtime-ready cancellation")
	}
	// The ACK precedes both fixture-only marker publications. The completion
	// marker follows the callback; fence both before starting the measured stop.
	waitShutdownMarker(t, f.cell.path("tmp", "cancel-handled"), shutdownFixtureLivenessBudget(f.startup))
	parentBudget := shutdownFixtureBudget(f.startup)
	ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
	defer cancel()
	started := time.Now()
	outcome := f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, 8*parentBudget)
	if outcome.Kind != serviceStopUnknown || outcome.Detail != "shutdown:deadline" || ctx.Err() != context.DeadlineExceeded {
		t.Logf("shutdown parent deadline class: %s", shutdownFixtureDeadlineClass(outcome, ctx.Err()))
		t.Logf("shutdown descriptor replay class: %s", f.descriptorClass())
		t.Fatal("parent exhaustion did not raise at the actual shutdown deadline")
	}
	// The allowance scales with this fixture's floored shutdown budget. A
	// detached app wait consumes eight parent budgets and must fail this check.
	if time.Since(started) >= 4*parentBudget {
		t.Fatal("app wait detached from the whole-tree deadline")
	}
	if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
		t.Fatal("whole-tree exhaustion permitted force")
	}
}

// Only missing publication and the exact sharing class are transient. A policy,
// reparse, access or other I/O refusal never authorizes another private read.
func windowsShutdownProbeReadClass(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "missing"
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return "sharing"
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, os.ErrPermission):
		return "access"
	default:
		return "other"
	}
}

func TestWindowsShutdownProbeReadClass(t *testing.T) {
	for _, row := range []struct {
		err   error
		class string
	}{
		{os.ErrNotExist, "missing"},
		{windows.ERROR_FILE_NOT_FOUND, "missing"},
		{windows.ERROR_PATH_NOT_FOUND, "missing"},
		{windows.ERROR_SHARING_VIOLATION, "sharing"},
		{errors.Join(errors.New("synthetic wrapper"), windows.ERROR_SHARING_VIOLATION), "sharing"},
		{windows.ERROR_ACCESS_DENIED, "access"},
		{windows.ERROR_LOCK_VIOLATION, "other"},
		{errors.New("private Windows sharing-violation"), "other"},
	} {
		if windowsShutdownProbeReadClass(row.err) != row.class {
			t.Fatal("probe read retry classifier accepted an inexact Windows error class")
		}
	}
}
