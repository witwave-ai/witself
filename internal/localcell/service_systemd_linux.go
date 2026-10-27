//go:build linux

package localcell

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// NewServiceAdapter returns the current-user Linux manager adapter. Constructing
// or planning an adapter never creates directories, starts a host, or enables
// lingering. An absent session has the explicit foreground-host fallback.
func NewServiceAdapter() ServiceAdapter {
	current, err := user.Current()
	if err != nil {
		return unavailableServiceAdapter{}
	}
	a := &systemdUserAdapter{uid: strconv.Itoa(os.Geteuid()), home: current.HomeDir, runtimeDir: os.Getenv("XDG_RUNTIME_DIR")}
	a.unitDir = filepath.Join(a.home, ".config", "systemd", "user")
	a.systemctl = systemdTool("systemctl")
	a.busctl = systemdTool("busctl")
	a.owned = func(_ string, info os.FileInfo) bool {
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && stat.Uid == uint32(os.Geteuid())
	}
	a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		command := exec.CommandContext(bounded, binary, args...)
		// Never copy the caller's environment or credentials into manager commands.
		command.Env = []string{"XDG_RUNTIME_DIR=" + a.runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + a.runtimeDir + "/bus"}
		return command.Output()
	}
	return a
}

func systemdTool(name string) string {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}
