//go:build windows

package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The helper runs the native test .exe in an isolated console; neither failure
// nor a process-group mutation can deliver CTRL_BREAK to the test runner.
func TestWindowsConsoleControlHelperProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "localcell-windows-control" {
		return
	}
	mode, root := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	c, err := New(Config{Root: root, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "wait", "wait-bystander":
		marker := "control"
		if mode == "wait-bystander" {
			marker = "bystander"
		}
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
		if err := writeAtomicBytes(c.path("tmp", marker+"-ready"), []byte("ready"), nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-interrupt:
			if err := writeAtomicBytes(c.path("tmp", marker+"-stopped"), []byte("interrupt"), nil); err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("console fixture never received interrupt")
		}
	case "launch", "launch-with-bystander":
		_, _, _ = freeConsoleProc.Call()
		alloc := windows.NewLazySystemDLL("kernel32.dll").NewProc("AllocConsole")
		if result, _, _ := alloc.Call(); result == 0 {
			t.Fatal("cannot establish test-owned console")
		}
		defer func() { _, _, _ = freeConsoleProc.Call() }()
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		environment, err := c.baseEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		log, err := c.logFile("server")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = log.Close() }()
		// A successful launcher transfers both children to the outer test.
		// If either setup fails, kill every already-published fixture by its
		// exact creation identity before releasing the test-owned console.
		complete := false
		defer func() {
			if complete {
				return
			}
			for _, component := range []string{"server", "worker"} {
				var record ProcessRecord
				if readJSON(c.path("run", component+".json"), &record) != nil {
					continue
				}
				handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
				if err != nil || !alive {
					continue
				}
				_ = windows.TerminateProcess(handle, 125)
				_, _ = windows.WaitForSingleObject(handle, 5000)
				_ = windows.CloseHandle(handle)
			}
		}()
		// Exercise the production suspended/job/publication path. Removing its
		// process-group flag must make the graceful stop witness fail as well.
		args := []string{"-test.run=^TestWindowsConsoleControlHelperProcess$", "--", "localcell-windows-control", "wait", root}
		if err := c.launchWindows(self, args, "server", environment, log, nil); err != nil {
			t.Fatal(err)
		}
		waitWindowsControlFile(t, c.path("tmp", "control-ready"))
		if mode == "launch-with-bystander" {
			// The same executable inherits this same console association but
			// launchWindows gives it a second, independent process group.
			args := []string{"-test.run=^TestWindowsConsoleControlHelperProcess$", "--", "localcell-windows-control", "wait-bystander", root}
			if err := c.launchWindows(self, args, "worker", environment, log, nil); err != nil {
				t.Fatal(err)
			}
			waitWindowsControlFile(t, c.path("tmp", "bystander-ready"))
		}
		complete = true
	case "stop":
		state, err := c.processState("server")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := c.terminateChild(ctx, "server", state); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown console fixture operation")
	}
}

func windowsControlFixtureCommand(t *testing.T, c *Cell, mode string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	systemRoot, err := windows.GetWindowsDirectory()
	if err != nil {
		t.Fatal("trusted Windows directory unavailable")
	}
	cmd := exec.Command(self, "-test.run=^TestWindowsConsoleControlHelperProcess$", "--", "localcell-windows-control", mode, c.cfg.Root)
	cmd.Env = []string{"SystemRoot=" + systemRoot}
	cmd.Dir = c.cfg.Root
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd
}

func waitWindowsControlFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("console fixture did not publish %s", filepath.Base(path))
}

func windowsControlFixture(t *testing.T, detached bool) (*Cell, ProcessRecord, windows.Handle) {
	t.Helper()
	return windowsControlFixtureMode(t, detached, "launch")
}

func windowsControlFixtureMode(t *testing.T, detached bool, launchMode string) (*Cell, ProcessRecord, windows.Handle) {
	t.Helper()
	c, _ := unitCell(t)
	if detached {
		cmd := windowsControlFixtureCommand(t, c, "wait")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		identity, alive, err := processIdentity(cmd.Process.Pid)
		if err != nil || !alive {
			t.Fatal("detached fixture identity unavailable")
		}
		record := ProcessRecord{PID: cmd.Process.Pid, StartTime: identity, Root: c.cfg.Root, Component: "server"}
		if err := writeAtomic(c.path("run", "server.json"), record, nil); err != nil {
			t.Fatal(err)
		}
		waitWindowsControlFile(t, c.path("tmp", "control-ready"))
	} else {
		launcher := windowsControlFixtureCommand(t, c, launchMode)
		launcher.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if err := launcher.Run(); err != nil {
			t.Fatal("console fixture launcher failed")
		}
	}
	var record ProcessRecord
	if err := readJSON(c.path("run", "server.json"), &record); err != nil {
		t.Fatal(err)
	}
	handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
	if err != nil || !alive {
		t.Fatal("console fixture did not outlive its launcher")
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(handle, 125)
		_, _ = windows.WaitForSingleObject(handle, 5000)
		_ = windows.CloseHandle(handle)
	})
	return c, record, handle
}

func TestWindowsStopGracefulAcrossConsoleAndInvocation(t *testing.T) {
	for _, laterInvocation := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-console", true: "later-invocation"}[laterInvocation], func(t *testing.T) {
			c, record, handle := windowsControlFixtureMode(t, false, "launch-with-bystander")
			var bystander ProcessRecord
			if err := readJSON(c.path("run", "worker.json"), &bystander); err != nil {
				t.Fatal(err)
			}
			bystanderHandle, alive, err := openVerifiedWindowsProcess(bystander, windows.PROCESS_TERMINATE)
			if err != nil || !alive || bystander.PID == record.PID {
				t.Fatal("console bystander did not outlive its launcher independently")
			}
			t.Cleanup(func() {
				_ = windows.TerminateProcess(bystanderHandle, 125)
				_, _ = windows.WaitForSingleObject(bystanderHandle, 5000)
				_ = windows.CloseHandle(bystanderHandle)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if laterInvocation {
				stop := windowsControlFixtureCommand(t, c, "stop")
				stop.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
				if err := stop.Run(); err != nil {
					t.Fatal("later invocation could not stop the proven component")
				}
			} else if err := c.terminateChild(ctx, "server", ProcessState{PID: record.PID, Alive: true, Matches: true}); err != nil {
				t.Fatal(err)
			}
			if result, err := windows.WaitForSingleObject(handle, 10000); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Fatal("graceful console stop did not terminate fixture")
			}
			stopped, err := os.ReadFile(c.path("tmp", "control-stopped"))
			if err != nil || string(stopped) != "interrupt" {
				t.Fatal("component exited without its graceful interrupt handler")
			}
			// Allow asynchronous console delivery to settle. Group 0 would also
			// interrupt this witness; checking only the target cannot detect it.
			if result, err := windows.WaitForSingleObject(bystanderHandle, 500); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				t.Fatal("targeted console stop terminated the unrelated process group")
			}
			if identity, alive, err := windowsProcessIdentity(bystanderHandle); err != nil || !alive || identity != bystander.StartTime {
				t.Fatal("targeted console stop lost the unrelated process identity")
			}
			if _, err := os.Stat(c.path("tmp", "bystander-stopped")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("targeted console stop interrupted the unrelated process group")
			}
			stopContext, stopCancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer stopCancel()
			if err := c.terminateChild(stopContext, "worker", ProcessState{PID: bystander.PID, Alive: true, Matches: true}); err != nil {
				t.Fatal(err)
			}
			if result, err := windows.WaitForSingleObject(bystanderHandle, 10000); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Fatal("explicit bystander stop did not terminate fixture")
			}
			stopped, err = os.ReadFile(c.path("tmp", "bystander-stopped"))
			if err != nil || string(stopped) != "interrupt" {
				t.Fatal("bystander exited without its explicit graceful stop")
			}
		})
	}
}

func TestWindowsStopRefusesAbsentConsole(t *testing.T) {
	c, record, handle := windowsControlFixture(t, true)
	err := c.terminateChild(t.Context(), "server", ProcessState{PID: record.PID, Alive: true, Matches: true})
	if !errors.Is(err, errWindowsConsoleUnavailable) {
		t.Fatal("detached component did not produce explicit console refusal")
	}
	if result, err := windows.WaitForSingleObject(handle, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("unsupported console stop terminated the component")
	}
}

func TestWindowsStopRechecksCreationFenceOnRetainedHandle(t *testing.T) {
	c, record, handle := windowsControlFixture(t, false)
	for _, pending := range []bool{false, true} {
		forged := record
		forged.StartTime = "windows:0:1"
		forged.LaunchPending = pending
		if err := writeAtomic(c.path("run", "server.json"), forged, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.terminateChild(t.Context(), "server", ProcessState{PID: record.PID, Alive: true, Matches: true}); err == nil {
			t.Fatal("PID match alone granted process control")
		}
		if result, err := windows.WaitForSingleObject(handle, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatal("forged creation identity signaled unrelated process")
		}
	}
	if err := signalTerminate(record.PID); err == nil {
		t.Fatal("PID-only signal bypassed durable identity validation")
	}
}

func TestWindowsPendingLaunchCleanupUsesExactHandle(t *testing.T) {
	c, record, handle := windowsControlFixture(t, true)
	record.LaunchPending = true
	if err := writeAtomic(c.path("run", "server.json"), record, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := c.terminateChild(ctx, "server", ProcessState{PID: record.PID, Alive: true, Matches: true}); err != nil {
		t.Fatal(err)
	}
	if result, err := windows.WaitForSingleObject(handle, 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatal("pending launch retained a live process after cleanup")
	}
	if _, err := os.Stat(c.path("tmp", "control-stopped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending launch cleanup unexpectedly used console signaling")
	}
}

func TestWindowsConfigureRequiresDedicatedProcessGroup(t *testing.T) {
	cmd := exec.Command("fixture.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	configureProcessCommand(cmd)
	if cmd.SysProcAttr.CreationFlags != windows.CREATE_SUSPENDED|windows.CREATE_NEW_PROCESS_GROUP {
		t.Fatal("component process group isolation is absent or discarded existing flags")
	}
}

func TestWindowsIdentityErrorsAreValueFree(t *testing.T) {
	_, _, err := openVerifiedWindowsProcess(ProcessRecord{PID: os.Getpid(), StartTime: "sentinel-untrusted-identity"}, 0)
	if err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatal("invalid control identity was accepted or disclosed")
	}
}

func TestWindowsIdentityRefusesAccessDenied(t *testing.T) {
	// A real console makes an accidental stop observable; a detached child
	// could refuse console control even if the identity fence were bypassed.
	c, record, handle := windowsControlFixture(t, false)
	originalOpen := windowsOpenProcess
	t.Cleanup(func() { windowsOpenProcess = originalOpen })
	queries := 0
	var requestedAccess uint32
	windowsOpenProcess = func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
		queries++
		requestedAccess = access
		if pid != uint32(record.PID) || inherit {
			t.Error("identity query used an unexpected process or inheritable handle")
		}
		return 0, windows.ERROR_ACCESS_DENIED
	}
	assertWindowsAccessDenied(t, c, record, handle, func(t *testing.T, access uint32) {
		t.Helper()
		if queries != 1 || requestedAccess != access {
			t.Error("refusal did not query the expected process access exactly once")
		}
		queries = 0
	})
}

// This integration witness complements the unconditional injected refusal.
// OpenProcess bypasses the process DACL when SeDebugPrivilege is enabled;
// administrator group membership alone does not justify skipping this test.
func TestWindowsIdentityRefusesAccessDeniedDACL(t *testing.T) {
	if windowsDebugPrivilegeEnabled(t) {
		t.Skip("LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege")
	}
	c, record, handle := windowsControlFixture(t, false)
	aclHandle, err := windows.OpenProcess(windows.WRITE_DAC, false, uint32(record.PID))
	if err != nil {
		t.Fatal("cannot prepare test-owned process ACL")
	}
	defer func() { _ = windows.CloseHandle(aclHandle) }()
	// Deny new identity queries to everyone. The cleanup handle was retained
	// before this change, so even this intentional refusal has a cleanup owner.
	sd, err := windows.SecurityDescriptorFromString("D:P(D;;0x00101000;;;WD)")
	if err != nil {
		t.Fatal("cannot construct test process ACL")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal("cannot extract test process ACL")
	}
	if err := windows.SetSecurityInfo(aclHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal("cannot apply test process ACL")
	}
	probe, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(record.PID))
	if err == nil {
		_ = windows.CloseHandle(probe)
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatal("test process DACL did not produce access denied with debug privilege disabled")
	}
	assertWindowsAccessDenied(t, c, record, handle, nil)
}

func windowsDebugPrivilegeEnabled(t *testing.T) bool {
	t.Helper()
	name, err := windows.UTF16PtrFromString("SeDebugPrivilege")
	if err != nil {
		t.Fatal("cannot encode debug privilege name")
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		t.Fatal("cannot resolve debug privilege")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal("cannot query test process privileges")
	}
	defer func() { _ = token.Close() }()
	// PRIVILEGE_SET with PRIVILEGE_SET_ALL_NECESSARY and one requested LUID.
	required := struct {
		count      uint32
		control    uint32
		privileges [1]windows.LUIDAndAttributes
	}{1, 1, [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
	var enabled int32
	check := windows.NewLazySystemDLL("advapi32.dll").NewProc("PrivilegeCheck")
	if ok, _, _ := check.Call(uintptr(token), uintptr(unsafe.Pointer(&required)), uintptr(unsafe.Pointer(&enabled))); ok == 0 {
		t.Fatal("cannot determine whether debug privilege is enabled")
	}
	return enabled != 0
}

func assertWindowsAccessDenied(t *testing.T, c *Cell, record ProcessRecord, handle windows.Handle, checkOpen func(*testing.T, uint32)) {
	t.Helper()
	for _, operation := range []string{"identity", "epoch", "stop", "pending-stop"} {
		t.Run(operation, func(t *testing.T) {
			access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
			switch operation {
			case "identity":
				if identity, alive, err := processIdentity(record.PID); err == nil || alive || identity != "" {
					t.Error("access-denied process identity was accepted")
				}
			case "epoch":
				if started, err := processStartedAt(record.PID); err == nil || !started.IsZero() {
					t.Error("access-denied process epoch was accepted")
				}
			default:
				record.LaunchPending = operation == "pending-stop"
				if record.LaunchPending {
					access |= windows.PROCESS_TERMINATE
				}
				if err := writeAtomic(c.path("run", "server.json"), record, nil); err != nil {
					t.Fatal("cannot prepare access-denied control record")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := c.terminateChild(ctx, "server", ProcessState{PID: record.PID, Alive: true, Matches: true}); err == nil {
					t.Error("access-denied identity permitted process control")
				}
			}
			if checkOpen != nil {
				checkOpen(t, access)
			}
			// This independently retained handle remains usable after the DACL
			// change or injected open failure, including for fixture cleanup.
			if result, err := windows.WaitForSingleObject(handle, 100); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				t.Error("access-denied identity signaled component")
			}
			if _, err := os.Stat(c.path("tmp", "control-stopped")); !errors.Is(err, os.ErrNotExist) {
				t.Error("access-denied identity delivered a console interrupt")
			}
		})
	}
}

func TestWindowsIdentityRefusesInvalidHandle(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		handle windows.Handle
	}{{"null", 0}, {"invalid-handle-value", windows.InvalidHandle}, {"current-process-pseudo-handle", windows.CurrentProcess()}} {
		t.Run(fixture.name, func(t *testing.T) {
			if identity, alive, err := windowsProcessIdentity(fixture.handle); err == nil || alive || identity != "" {
				t.Fatal("invalid or pseudo process handle was accepted")
			}
		})
	}
	t.Run("closed-process-handle", func(t *testing.T) {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))
		if err != nil {
			t.Fatal("cannot open real process handle")
		}
		identity, alive, identityErr := windowsProcessIdentity(handle)
		if err := windows.CloseHandle(handle); err != nil {
			t.Fatal("cannot close real process handle")
		}
		if identityErr != nil || !alive || identity == "" {
			t.Fatal("real process handle was not queryable before close")
		}
		if _, err := windows.WaitForSingleObject(handle, 0); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatal("closed process handle did not produce invalid handle")
		}
		if identity, alive, err := windowsProcessIdentity(handle); err == nil || alive || identity != "" {
			t.Fatal("closed process handle was accepted")
		}
	})
}

func TestWindowsRepairTerminatesPublishedSuspendedChild(t *testing.T) {
	c, m := unitCell(t)
	c.cfg.Timeout = 10 * time.Second
	c.cfg.Binaries.Server = buildWindowsLaunchFixture(t)
	if c.makePassword() != nil || c.journal("start", "operation", "started") != nil || c.journal("start", "server", "started") != nil {
		t.Fatal("cannot prepare suspended recovery fixture")
	}
	originalCheckpoint := windowsLaunchCheckpoint
	t.Cleanup(func() { windowsLaunchCheckpoint = originalCheckpoint })
	interrupted := errors.New("synthetic recovered suspension")
	repaired := false
	windowsLaunchCheckpoint = func(stage string, process windows.ProcessInformation) error {
		if stage != "published" {
			return nil
		}
		if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
			t.Error("suspended recovery fixture ran before Repair")
		}
		if err := c.Repair(t.Context()); err != nil {
			t.Error("Repair refused its published suspended process")
			return interrupted
		}
		if state, err := windows.WaitForSingleObject(process.Process, 0); err != nil || state != windows.WAIT_OBJECT_0 {
			t.Error("Repair left its suspended component alive")
		}
		progress, err := c.readJournal()
		if err != nil || progress.pending != nil {
			t.Error("Repair did not settle interrupted launch journal")
		}
		repaired = true
		return interrupted
	}
	if err := c.startChild(m, "server"); !errors.Is(err, interrupted) || !repaired {
		t.Fatal("suspended child did not reach verified Repair cleanup")
	}
	if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Repair resumed a pending component")
	}
}
