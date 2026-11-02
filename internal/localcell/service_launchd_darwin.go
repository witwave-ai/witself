//go:build darwin

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

// NewServiceAdapter selects only the owning user's GUI LaunchAgent domain.
// Construction has no filesystem or manager mutations; Apply is explicit opt-in.
func NewServiceAdapter() ServiceAdapter {
	home, _, _ := systemdUserDirectories(os.UserHomeDir, func() (string, error) {
		current, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if err != nil {
			return "", err
		}
		return current.HomeDir, nil
	}, "")
	a := &launchdAgentAdapter{uid: strconv.Itoa(os.Getuid()), home: home, unavailable: os.Getuid() != os.Geteuid()}
	if home != "" {
		a.agentDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	if info, err := os.Stat("/bin/launchctl"); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		a.launchctl = "/bin/launchctl"
	}
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
		command.Env = []string{"HOME=" + a.home}
		return command.Output()
	}
	return a
}
