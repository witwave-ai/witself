//go:build !windows

package localcell

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const childLaunchArgument = "--localcell-internal-exec"

// The package is linked only into tests and the explicit harness. Re-exec gives
// the child a chance to publish its own PID fence BEFORE exec replaces it with
// the real component, closing the parent's spawn-before-record crash window.
// It receives the same scratch environment that the final binary will receive.
func init() {
	if len(os.Args) != 5 || os.Args[1] != childLaunchArgument {
		return
	}
	root, component, binary := os.Args[2], os.Args[3], os.Args[4]
	if component != "server" && component != "worker" {
		os.Exit(125)
	}
	c, err := New(Config{Root: root})
	if err != nil || c.validateRoot(false) != nil {
		os.Exit(125)
	}
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		os.Exit(125)
	}
	record := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: component}
	if writeAtomic(c.path("run", component+".json"), record, nil) != nil {
		os.Exit(125)
	}
	// A crashed parent closes the pipe: the helper then exits without ever
	// starting a component. Publication must precede the parent's permission.
	var permit [1]byte
	if _, err := io.ReadFull(os.Stdin, permit[:]); err != nil || permit[0] != 1 {
		os.Exit(125)
	}
	if syscall.Exec(binary, []string{binary, "serve"}, os.Environ()) != nil {
		os.Exit(125)
	}
}

func childCommand(binary, root, component string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot locate local cell launch helper")
	}
	return exec.Command(self, childLaunchArgument, root, component, binary), nil
}

func (c *Cell) launchChild(binary, component string, environment []string, log *os.File) error {
	// Deliberately not CommandContext: a successful Start transfers lifetime to
	// the persisted PID record, independent of the invoking harness's lifetime.
	cmd, err := childCommand(binary, c.cfg.Root, component)
	if err != nil {
		return err
	}
	cmd.Env, cmd.Dir = environment, c.cfg.Root
	cmd.Stdout, cmd.Stderr = log, log
	permit, err := cmd.StdinPipe()
	if err != nil {
		return errors.New("cannot create local cell launch handshake")
	}
	defer func() { _ = permit.Close() }()
	configureProcessCommand(cmd)
	if err := cmd.Start(); err != nil {
		return errors.New("cannot launch local cell component")
	}
	_ = c.observeChildExit(component, cmd.Process.Pid, func() (*os.ProcessState, error) {
		err := cmd.Wait()
		return cmd.ProcessState, err
	})
	identity, alive, err := processIdentity(cmd.Process.Pid)
	if err != nil || !alive {
		return errors.New("cannot identify launched local cell component")
	}
	deadline := time.Now().Add(c.cfg.Timeout)
	for time.Now().Before(deadline) {
		state, stateErr := c.processState(component)
		if stateErr == nil && state.PID == cmd.Process.Pid && state.Alive && state.Matches {
			if _, err := permit.Write([]byte{1}); err != nil {
				return errors.New("cannot authorize local cell component launch")
			}
			return nil
		}
		current, alive, identityErr := processIdentity(cmd.Process.Pid)
		if identityErr != nil || !alive || current != identity {
			return errors.New("local cell launch helper exited before publishing ownership")
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, alive, identityErr := processIdentity(cmd.Process.Pid)
	if identityErr == nil && alive && current == identity {
		_ = signalTerminate(cmd.Process.Pid)
	}
	return errors.New("local cell launch ownership deadline exceeded")
}
