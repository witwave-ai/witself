//go:build windows

package localcell_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
	"golang.org/x/sys/windows"
)

// The explicit Windows host has no console. Authenticated exit-host drains its
// children and exits without generating a console control event.
func nativeServicePrepareForegroundCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
}

func nativeServiceInterruptForeground(process *os.Process) error {
	if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(process.Pid)) != nil {
		return errors.New("native foreground console interrupt unavailable")
	}
	return nil
}

func nativeServicePlatformCapabilityDiagnostic() []byte {
	return []byte(localcell.NativeServiceBootDiagnostic())
}

// Retain only the exact endpoint chosen by this host; absence of its private
// record alone would not prove that every named-pipe instance was closed.
func nativeServiceEndpointClosureProof(t *testing.T, root string) func() {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "service", "control-endpoint.json"))
	var endpoint struct {
		PipeName string `json:"pipe_name"`
	}
	if err != nil || json.Unmarshal(data, &endpoint) != nil || endpoint.PipeName == "" {
		t.Fatal("cannot retain native pipe closure witness")
	}
	name, err := windows.UTF16PtrFromString(endpoint.PipeName)
	if err != nil {
		t.Fatal("cannot encode native pipe closure witness")
	}
	return func() {
		t.Helper()
		handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			_ = windows.CloseHandle(handle)
			t.Fatal("native host retained a pipe instance after shutdown")
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatal("native pipe closure could not be classified absent")
		}
	}
}
