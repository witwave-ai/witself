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
		if len(args) != 3 {
			os.Exit(71)
		}
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(72)
		}
		result, _, callErr := attachConsoleProc.Call(uintptr(pid))
		if result != 0 {
			_, _, _ = freeConsoleProc.Call()
			os.Exit(73)
		}
		if !errors.Is(callErr, windows.ERROR_INVALID_HANDLE) {
			os.Exit(74)
		}
		if writeAtomicBytes(args[2], []byte("absent"), nil) != nil {
			os.Exit(75)
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
	getConsole := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	window, _, _ := getConsole.Call()
	if window != 0 {
		os.Exit(77)
	}
	if writeAtomicBytes(filepath.Join(root, "tmp", "console-absent"), []byte("absent"), nil) != nil {
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
		_ = writeAtomicBytes(filepath.Join(root, "tmp", "cancel-handled"), []byte(strconv.Itoa(completed)), nil)
	}
	var cancelCalls atomic.Uint32
	var cancelMu sync.Mutex
	requestCancel := func() {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		_ = writeAtomicBytes(filepath.Join(root, "tmp", "cancel-requested"), []byte(strconv.FormatUint(uint64(cancelCalls.Add(1)), 10)), nil)
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
	if writeAtomicBytes(filepath.Join(root, "tmp", "store-entered"), []byte("entered"), nil) != nil {
		os.Exit(80)
	}
	if mode != "migration" {
		if channel.RuntimeReady() != nil {
			os.Exit(81)
		}
	}
	if writeAtomicBytes(filepath.Join(root, "tmp", "runtime-started"), []byte("started"), nil) != nil {
		os.Exit(82)
	}
	<-ctx.Done()
	_ = channel.Close()
	os.Exit(0)
}

type windowsShutdownFixture struct {
	cell    *Cell
	record  ProcessRecord
	handle  windows.Handle
	startup time.Duration
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
	c.cfg.Timeout = 20 * shutdownFixtureStartup(t, c)
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
	started := time.Now()
	err = c.launchWindows(os.Args[0], []string{"-test.run=^TestWindowsShutdownChildHelper$", "--", mode}, "server", environment, log, nil)
	if (err != nil) != barrier {
		t.Fatal("synthetic launch did not reach expected release fence")
	}
	var record ProcessRecord
	if readJSON(c.path("run", "server.json"), &record) != nil || record.LaunchKind != shutdownLaunchPipe {
		t.Fatal("synthetic pipe record unavailable")
	}
	if !barrier {
		waitShutdownMarker(t, c.path("tmp", "runtime-started"), 10*time.Since(started))
	}
	return windowsShutdownFixture{c, record, handle, time.Since(started)}
}

func waitShutdownMarker(t *testing.T, path string, budget time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	for {
		if _, err := os.Stat(path); err == nil {
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*f.startup+time.Second)
	defer cancel()
	return f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, max(2*f.startup, 25*time.Millisecond))
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
	f := launchShutdownFixture(t, "normal", false)
	if _, err := os.Stat(f.cell.path("tmp", "console-absent")); err != nil {
		t.Fatal("child did not prove absent console window")
	}
	probe := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsShutdownChildHelper$", "--", "probe", strconv.Itoa(f.record.PID), f.cell.path("tmp", "probe-absent"))
	probe.Env, _ = f.cell.baseEnvironment()
	probe.Dir = f.cell.cfg.Root
	probe.Stdout, probe.Stderr = io.Discard, io.Discard
	probe.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	if probe.Run() != nil {
		t.Fatal("detached AttachConsole probe did not prove ERROR_INVALID_HANDLE")
	}
	if _, err := os.Stat(f.cell.path("tmp", "probe-absent")); err != nil {
		t.Fatal("no-console probe witness absent")
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
			t.Fatal("descriptor unavailable")
		}
		id, err := shutdownRandom(16)
		if err != nil {
			t.Fatal("request fixture unavailable")
		}
		request := d.request("cancel", id)
		first, err := windowsShutdownRequest(t.Context(), d, request)
		if err != nil {
			t.Fatal("clean cancel request refused")
		}
		second, err := windowsShutdownRequest(t.Context(), d, request)
		if err != nil || first != second {
			t.Fatal("retried cancel did not return identical ACK")
		}
		// The second ACK can arrive before its deferred callback. Observe handler
		// completion before reading the callback count, so a removed once guard fails.
		completion, cancel := context.WithTimeout(t.Context(), 2*f.startup)
		defer cancel()
		for {
			handled, err := readPrivateFile(f.cell.path("tmp", "cancel-handled"))
			if err == nil && string(handled) == "2" {
				break
			}
			select {
			case <-completion.Done():
				t.Fatal("second cancel handler did not complete")
			case <-time.After(f.startup / 100):
			}
		}
		data, err := readPrivateFile(f.cell.path("tmp", "cancel-requested"))
		if err != nil || string(data) != "1" {
			t.Fatal("retry invoked cancellation more than once")
		}
		request.Action = "release"
		if _, err := windowsShutdownRequest(t.Context(), d, request); err == nil {
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
		outcome := f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, max(2*f.startup, 25*time.Millisecond))
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
	parentBudget := 4 * f.startup
	ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
	defer cancel()
	started := time.Now()
	outcome := f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, 8*parentBudget)
	if outcome.Kind != serviceStopUnknown || outcome.Detail != "shutdown:deadline" || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("parent exhaustion did not raise at the actual shutdown deadline")
	}
	// The allowance scales with this fixture's measured process startup. A
	// detached app wait consumes eight parent budgets and must fail this check.
	if time.Since(started) >= 4*parentBudget {
		t.Fatal("app wait detached from the whole-tree deadline")
	}
	if _, alive, err := windowsProcessIdentity(f.handle); err != nil || !alive {
		t.Fatal("whole-tree exhaustion permitted force")
	}
}
