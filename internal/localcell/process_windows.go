//go:build windows

package localcell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Keep identity and control on the same OpenProcess boundary. Tests inject an
// access refusal here because enabled SeDebugPrivilege bypasses process DACLs.
var windowsOpenProcess = windows.OpenProcess

// OpenProcessToken / GetTokenInformation (TOKEN_USER) verify the retained
// process object, not its default owner or a second PID lookup.
var windowsProcessTokenUser = func(handle windows.Handle) (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return nil, errors.New("localcell: peer:sid")
	}
	defer func() { _ = token.Close() }()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, errors.New("localcell: peer:sid")
	}
	return user.User.Sid.Copy()
}

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

// The queried image and its owner come from the retained process object. File
// identity is compared through open file handles, never a textual path match or
// a second PID lookup. The handles deny replacement while being compared.
func windowsValidateProcessExecutable(handle windows.Handle, expected string) error {
	if handle == 0 || handle == windows.InvalidHandle || !filepath.IsAbs(expected) {
		return errors.New("localcell: process executable identity refused")
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil || size == 0 || size >= uint32(len(buffer)) {
		return errors.New("localcell: process executable identity refused")
	}
	actual := windows.UTF16ToString(buffer[:size])
	identity := func(path string) (windows.Handle, windows.ByHandleFileInformation, error) {
		var info windows.ByHandleFileInformation
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return 0, info, err
		}
		file, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return 0, info, err
		}
		if err := windows.GetFileInformationByHandle(file, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
			_ = windows.CloseHandle(file)
			return 0, info, errors.New("localcell: process executable identity refused")
		}
		return file, info, nil
	}
	want, wantInfo, err := identity(expected)
	if err != nil {
		return errors.New("localcell: process executable identity refused")
	}
	defer func() { _ = windows.CloseHandle(want) }()
	got, gotInfo, err := identity(actual)
	if err != nil {
		return errors.New("localcell: process executable identity refused")
	}
	defer func() { _ = windows.CloseHandle(got) }()
	if gotInfo.VolumeSerialNumber != wantInfo.VolumeSerialNumber || gotInfo.FileIndexHigh != wantInfo.FileIndexHigh || gotInfo.FileIndexLow != wantInfo.FileIndexLow {
		return errors.New("localcell: process executable identity refused")
	}
	return nil
}
