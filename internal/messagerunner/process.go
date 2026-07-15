package messagerunner

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// configureProviderProcess places a provider and ordinary descendants in a
// separate process group so context cancellation kills more than the direct
// child. Provider CLIs still run without a shell and under their own bounded
// timeout.
func configureProviderProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
}
