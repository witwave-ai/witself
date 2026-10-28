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

	"golang.org/x/sys/unix"
)

// NewServiceAdapter returns the current-user Linux manager adapter. Constructing
// or planning an adapter never creates directories, starts a host, or enables
// lingering. An absent session has the explicit foreground-host fallback.
func NewServiceAdapter() ServiceAdapter {
	home, base, unitDir := systemdUserDirectories(os.UserHomeDir, func() (string, error) {
		current, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if err != nil {
			return "", err
		}
		return current.HomeDir, nil
	}, os.Getenv("XDG_CONFIG_HOME"))
	a := &systemdUserAdapter{uid: strconv.Itoa(os.Getuid()), home: home, unitBase: base, unitDir: unitDir, runtimeDir: os.Getenv("XDG_RUNTIME_DIR"), unavailable: os.Getuid() != os.Geteuid()}
	a.systemctl = systemdTool("systemctl")
	a.busctl = systemdTool("busctl")
	a.owned = func(_ string, info os.FileInfo) bool {
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && stat.Uid == uint32(os.Geteuid())
	}
	a.accessible = func(path string, write bool) bool {
		mode := uint32(unix.X_OK)
		if write {
			mode |= unix.W_OK
		}
		return unix.Access(path, mode) == nil
	}
	a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		command := exec.CommandContext(bounded, binary, args...)
		// Resolve config paths once, then explicitly carry that same search path
		// into systemctl enable. Never copy arbitrary caller values or credentials.
		command.Env = []string{"XDG_RUNTIME_DIR=" + a.runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + a.runtimeDir + "/bus", "HOME=" + a.home, "XDG_CONFIG_HOME=" + filepath.Dir(filepath.Dir(a.unitDir))}
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
