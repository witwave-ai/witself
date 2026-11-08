//go:build windows

package localcell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewServiceAdapter only constructs the task adapter. Registration still needs
// an explicitly selected windows-logon-task descriptor and a proven capability.
func NewServiceAdapter() ServiceAdapter { return NewWindowsLogonTaskAdapter() }

func NewWindowsLogonTaskAdapter() ServiceAdapter {
	uid, _ := serviceOwner()
	return &windowsLogonTaskAdapter{uid: uid, command: runWindowsTaskCommand, capability: WindowsLogonTaskCapability}
}

func validateServiceHostIdentity(expected string) error {
	handle, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	if err != nil {
		return ErrServiceOwnershipConflict
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if windowsValidateProcessExecutable(handle, expected) != nil {
		return ErrServiceOwnershipConflict
	}
	return nil
}

func windowsTaskTokenUint32(token windows.Token, class uint32) (uint32, error) {
	var value, size uint32
	err := windows.GetTokenInformation(token, class, (*byte)(unsafe.Pointer(&value)), uint32(unsafe.Sizeof(value)), &size)
	if err != nil || size != uint32(unsafe.Sizeof(value)) {
		return 0, errors.New("windows task token information unavailable")
	}
	return value, nil
}

func validateTaskHostOwner(uid string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	elevated, elevationErr := windowsTaskTokenUint32(token, windows.TokenElevation)
	if err != nil || user == nil || user.User.Sid == nil || elevationErr != nil ||
		user.User.Sid.String() != uid || !validWindowsTaskOwner(uid, elevated != 0) {
		return windowsTaskError(ErrServiceManagerUnavailable, "inspect:host_owner", 0, "owner_token_refused")
	}
	return nil
}

func windowsTaskInteractiveSession() error {
	token := windows.GetCurrentProcessToken()
	session, err := windowsTaskTokenUint32(token, windows.TokenSessionId)
	if err != nil || session == 0 {
		return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "interactive_token_unavailable")
	}
	groups, err := token.GetTokenGroups()
	if err != nil || groups == nil {
		return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "interactive_token_unavailable")
	}
	interactive, err := windows.CreateWellKnownSid(windows.WinInteractiveSid)
	if err != nil {
		return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "interactive_token_unavailable")
	}
	for _, group := range groups.AllGroups() {
		if group.Sid != nil && group.Sid.Equals(interactive) && group.Attributes&windows.SE_GROUP_ENABLED != 0 && group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			return nil
		}
	}
	return windowsTaskError(ErrServiceManagerUnavailable, "capability:session", 0, "interactive_token_unavailable")
}

func windowsTaskSystemCommand() (string, string, error) {
	directory, err := windows.GetSystemDirectory()
	root, rootErr := windows.GetWindowsDirectory()
	if err != nil || rootErr != nil || !filepath.IsAbs(directory) || !filepath.IsAbs(root) {
		return "", "", windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "system_directory_unavailable")
	}
	path := filepath.Join(directory, "schtasks.exe")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "system_command_unavailable")
	}
	return path, root, nil
}

// WindowsLogonTaskCapability never registers or starts anything. There is no
// native absent/access-denied/RPC code triplet in this tree. Until the Windows
// row proves that premise, no caller may interpret generic schtasks exit 1 or
// English stderr as absence, or reach a mutation through a successful Plan.
func WindowsLogonTaskCapability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := serviceOwner()
	if err != nil {
		return windowsTaskError(ErrServiceManagerUnavailable, "inspect:host_owner", 0, "owner_token_refused")
	}
	if err := validateTaskHostOwner(uid); err != nil {
		return err
	}
	if err := windowsTaskInteractiveSession(); err != nil {
		return err
	}
	if _, _, err := windowsTaskSystemCommand(); err != nil {
		return err
	}
	return windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "query_result_untyped")
}

func runWindowsTaskCommand(ctx context.Context, args ...string) (windowsTaskCommandResult, error) {
	var result windowsTaskCommandResult
	path, root, err := windowsTaskSystemCommand()
	if err != nil {
		return result, err
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, path, args...)
	command.WaitDelay = time.Second // Bound inherited stdout/stderr handles too.
	command.Env = []string{"SystemRoot=" + root, "WINDIR=" + root}
	command.Dir = filepath.Dir(path)
	command.Stdin = nil // os/exec connects a nil stdin to the null device.
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	capture := &windowsTaskCapture{}
	command.Stdout, command.Stderr = capture.stdout(), capture.stderr()
	err = command.Run()
	if capture.exceeded() {
		return result, windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "output_limit")
	}
	if command.ProcessState != nil {
		result.Code = uint32(command.ProcessState.ExitCode())
	}
	if bounded.Err() != nil {
		return result, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "command_timeout")
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		return result, windowsTaskError(ErrServiceManagerUnavailable, "query:result", result.Code, "command_failed")
	}
	// Only stdout can become XML; stderr is bounded and discarded, never used
	// for classification or included in a diagnostic. Typed deliberately stays
	// false until the native query-code fixture has proved all three failures.
	result.Output = capture.output()
	return result, nil
}
