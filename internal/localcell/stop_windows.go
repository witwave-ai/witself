//go:build windows

package localcell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const windowsStopArgument = "--localcell-internal-stop"

var (
	attachConsoleProc            = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	freeConsoleProc              = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
	errWindowsConsoleUnavailable = errors.New("localcell: component console unavailable; graceful stop refused")
)

// A separate detached helper attaches only to the verified target's console.
// It leaves the invoking harness's console unchanged, including when that
// harness is a later invocation in a different terminal. CTRL_BREAK is scoped
// to the component's CREATE_NEW_PROCESS_GROUP, and Go maps it to os.Interrupt,
// which both server and worker already handle. A closed or absent target console
// is an explicit refusal, not authority to force-kill a running component.
func init() {
	if len(os.Args) != 6 || os.Args[1] != windowsStopArgument {
		return
	}
	pid, err := strconv.Atoi(os.Args[4])
	if err != nil || (os.Args[3] != "server" && os.Args[3] != "worker") {
		os.Exit(125)
	}
	c, err := New(Config{Root: os.Args[2]})
	if err != nil || c.validateRoot(false) != nil {
		os.Exit(125)
	}
	record, err := c.windowsStopRecord(os.Args[3], ProcessState{PID: pid, Alive: true, Matches: true})
	if err != nil || record.StartTime != os.Args[5] || record.LaunchPending {
		os.Exit(125)
	}
	handle, alive, err := openVerifiedWindowsProcess(record, 0)
	if err != nil {
		os.Exit(125)
	}
	if !alive {
		os.Exit(0)
	}
	code := windowsConsoleBreak(handle, record)
	_ = windows.CloseHandle(handle)
	os.Exit(code)
}

func (c *Cell) windowsStopRecord(component string, state ProcessState) (ProcessRecord, error) {
	var record ProcessRecord
	if component != "server" && component != "worker" && component != "postgres" {
		return record, errors.New("localcell: invalid component for process control")
	}
	if err := readJSON(c.path("run", component+".json"), &record); err != nil {
		return record, errors.New("localcell: cannot read process control identity")
	}
	if !state.Alive || !state.Matches || record.PID != state.PID || record.PID <= 1 ||
		record.StartTime == "" || record.Root != c.cfg.Root || record.Component != component {
		return record, errors.New("localcell: process identity mismatch; refusing to signal")
	}
	return record, nil
}

func openVerifiedWindowsProcess(record ProcessRecord, access uint32) (windows.Handle, bool, error) {
	if record.PID <= 1 || uint64(record.PID) > uint64(^uint32(0)) || record.StartTime == "" {
		return 0, false, errors.New("localcell: invalid process control identity")
	}
	handle, err := windowsOpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(record.PID))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errors.New("localcell: cannot open process control identity")
	}
	identity, alive, err := windowsProcessIdentity(handle)
	if err != nil || !alive || identity != record.StartTime {
		_ = windows.CloseHandle(handle)
		if err == nil && !alive {
			return 0, false, nil
		}
		return 0, false, errors.New("localcell: process creation identity mismatch; refusing to signal")
	}
	return handle, true, nil
}

func windowsConsoleBreak(handle windows.Handle, record ProcessRecord) int {
	if result, _, _ := attachConsoleProc.Call(uintptr(record.PID)); result == 0 {
		return 124
	}
	defer func() { _, _, _ = freeConsoleProc.Call() }()
	identity, alive, err := windowsProcessIdentity(handle)
	if err != nil || (alive && identity != record.StartTime) {
		return 125
	}
	if !alive {
		return 0
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(record.PID)); err != nil {
		return 124
	}
	return 0
}

func (c *Cell) terminateChild(ctx context.Context, component string, state ProcessState) error {
	record, err := c.windowsStopRecord(component, state)
	if err != nil {
		return err
	}
	if record.LaunchPending {
		return terminateWindowsPending(ctx, record)
	}
	handle, alive, err := openVerifiedWindowsProcess(record, 0)
	if err != nil || !alive {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	self, err := os.Executable()
	if err != nil {
		return errors.New("localcell: cannot locate console control helper")
	}
	environment, err := c.baseEnvironment()
	if err != nil {
		return errors.New("localcell: cannot establish console helper environment")
	}
	cmd := exec.CommandContext(ctx, self, windowsStopArgument, c.cfg.Root, component, strconv.Itoa(record.PID), record.StartTime)
	cmd.Env = environment
	cmd.Dir = c.cfg.Root
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 124 {
			return errWindowsConsoleUnavailable
		}
		return errors.New("localcell: verified console stop failed")
	}
	return nil
}

func (c *Cell) terminatePendingPostgres(ctx context.Context, state ProcessState) (bool, error) {
	record, err := c.windowsStopRecord("postgres", state)
	if err != nil {
		return false, err
	}
	if !record.LaunchPending {
		return false, nil
	}
	return true, terminateWindowsPending(ctx, record)
}

func (c *Cell) stopPostgres(ctx context.Context, state ProcessState) error {
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	record, err := c.windowsStopRecord("postgres", state)
	if err != nil {
		return err
	}
	handle, alive, err := openVerifiedWindowsProcess(record, 0)
	if err != nil || !alive {
		return err
	}
	// Keeping this object open reserves its PID across pg_ctl's own PID-file
	// lookup, so a postmaster exit cannot redirect the request to a reused PID.
	defer func() { _ = windows.CloseHandle(handle) }()
	pid, err := c.postmaster()
	if err != nil || pid != record.PID {
		return errors.New("localcell: PostgreSQL process identity mismatch; refusing to stop")
	}
	if err := c.pgCommand(ctx, "pg_ctl", "stop", "-D", c.path("pg"), "-m", "fast", "-w", "-t", strconv.Itoa(max(1, int(c.cfg.Timeout.Seconds())))); err != nil {
		return err
	}
	// pg_ctl can finish before the postmaster's process object signals exit.
	// Keep the verified handle and use the same deadline, including time already
	// spent in pg_ctl, so successful Stop permits immediate removal of its files.
	deadline, _ := ctx.Deadline()
	for {
		remaining := time.Until(deadline)
		wait := uint32(max(0, min(25, remaining.Milliseconds())))
		result, err := windows.WaitForSingleObject(handle, wait)
		if err != nil || (result != windows.WAIT_OBJECT_0 && result != uint32(windows.WAIT_TIMEOUT)) {
			return errors.New("localcell: cannot verify PostgreSQL stop completion")
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if ctx.Err() != nil || remaining <= 0 {
			return fixedCause("PostgreSQL did not exit after stop")
		}
	}
}

// An unfinished launch has not earned graceful shutdown semantics. Repair owns
// its exact retained process object whether the thread remains suspended or was
// resumed just before the final publication failed.
func terminateWindowsPending(ctx context.Context, record ProcessRecord) error {
	handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
	if err != nil || !alive {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.TerminateProcess(handle, 125); err != nil {
		return errors.New("localcell: cannot terminate unfinished component launch")
	}
	for {
		result, err := windows.WaitForSingleObject(handle, 25)
		if err != nil {
			return errors.New("localcell: cannot verify unfinished launch cleanup")
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("localcell: unknown unfinished launch cleanup state")
		}
		select {
		case <-ctx.Done():
			return errors.New("localcell: unfinished launch cleanup deadline exceeded")
		default:
		}
	}
}
