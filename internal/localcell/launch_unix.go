//go:build !windows

package localcell

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
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
