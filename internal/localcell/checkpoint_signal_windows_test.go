package localcell

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// checkpointSignalPrepare starts the child in a new process group, which
// Ctrl-Break (delivered as os.Interrupt) targets; GenerateConsoleCtrlEvent
// cannot raise console close, logoff or shutdown, so every row sends it.
func checkpointSignalPrepare(child *exec.Cmd) {
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func checkpointSignalSend(process *os.Process, _ string) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(process.Pid))
}
