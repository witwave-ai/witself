package main

import (
	"os"
	"os/exec"
	"slices"
	"syscall"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// lcaPrivateDir and lcaPrivateFile create objects with the protected DACL the
// local cell accepts as private; a runner's inherited ACLs are not private.
func lcaPrivateDir(path string) error { return runtimeconfig.WindowsPrivateMkdir(path) }

func lcaPrivateFile(path string, data []byte) error {
	file, err := runtimeconfig.WindowsPrivateOpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// lcsNames is the OS set: Ctrl-Break, delivered as os.Interrupt. No row can
// raise console close, logoff or shutdown (GenerateConsoleCtrlEvent cannot).
var lcsNames = []string{"interrupt"}

// lcsStart starts the child in a new process group, which Ctrl-Break targets.
func lcsStart(child *exec.Cmd, _ string) error {
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	return child.Start()
}

func lcsSend(process *os.Process, _ string) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(process.Pid))
}

// lcsEndedBy: the default Ctrl-Break handler's status, never the command's 0, 1 or 2.
func lcsEndedBy(state *os.ProcessState, _ string) bool {
	return !slices.Contains([]int{-1, 0, 1, 2}, state.ExitCode())
}
