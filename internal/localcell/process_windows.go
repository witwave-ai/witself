//go:build windows

package localcell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Keep identity and control on the same OpenProcess boundary. Tests inject an
// access refusal here because enabled SeDebugPrivilege bypasses process DACLs.
var windowsOpenProcess = windows.OpenProcess

func processIdentity(pid int) (string, bool, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return "", false, errors.New("localcell: invalid process ID")
	}
	handle, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errors.New("localcell: cannot verify process creation time")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windowsProcessIdentity(handle)
}

// windowsProcessIdentity uses the retained kernel object, never a second PID
// lookup. The same helper fences suspended publication and shutdown.
func windowsProcessIdentity(handle windows.Handle) (string, bool, error) {
	// INVALID_HANDLE_VALUE is also GetCurrentProcess's pseudo-handle. It can
	// answer process queries, but cannot fence a retained child kernel object.
	if handle == 0 || handle == windows.InvalidHandle {
		return "", false, errors.New("localcell: invalid process identity handle")
	}
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return "", false, errors.New("localcell: cannot verify process liveness")
	}
	if state == windows.WAIT_OBJECT_0 {
		return "", false, nil
	}
	if state != uint32(windows.WAIT_TIMEOUT) {
		return "", false, errors.New("localcell: unknown process liveness")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", false, errors.New("localcell: cannot verify process creation time")
	}
	return fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime), true, nil
}

func signalTerminate(pid int) error {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return errors.New("localcell: invalid process ID")
	}
	// PID-only console control cannot establish ownership. Windows callers must
	// use terminateChild, which retains and verifies the durable identity fence.
	return errors.New("localcell: process identity is required before signaling")
}

func processStartedAt(pid int) (time.Time, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return time.Time{}, errors.New("localcell: invalid process ID")
	}
	handle, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return time.Time{}, errors.New("localcell: cannot verify process creation time")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		return time.Time{}, errors.New("localcell: cannot verify live process creation time")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, errors.New("localcell: cannot verify process creation time")
	}
	return time.Unix(0, created.Nanoseconds()).UTC(), nil
}

func configureProcessCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func privateOwnedDir(path string, info os.FileInfo) bool {
	if !info.IsDir() {
		return false
	}
	return validatePrivate(path, true) == nil
}
