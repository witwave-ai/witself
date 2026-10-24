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

func processIdentity(pid int) (string, bool, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return "", false, errors.New("localcell: invalid process ID")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errors.New("localcell: cannot verify process creation time")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
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
	// Windows has no SIGTERM. A dedicated child process group permits the
	// console's graceful shutdown event without notifying unrelated processes.
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

func processStartedAt(pid int) (time.Time, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return time.Time{}, errors.New("localcell: invalid process ID")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
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
