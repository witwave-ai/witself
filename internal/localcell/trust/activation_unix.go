//go:build !windows

package trust

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

func privateOwned(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
}

func openHostExecutable(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
}

func hostFileIdentity(_ *os.File, info os.FileInfo) (HostIdentity, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return HostIdentity{}, ReasonPayloadIdentity
	}
	return HostIdentity{Device: uint64(s.Dev), Inode: uint64(s.Ino)}, nil
}

func generationProcessReferenced(ctx context.Context, host string) (bool, error) {
	// Darwin has no standard-library process executable identity enumeration.
	// Never shell out to ps or infer that unsupported visibility means no users.
	if runtime.GOOS != "linux" {
		return false, ReasonProcessObservationUnavailable
	}
	want, err := os.Stat(host)
	if err != nil {
		return false, ReasonProcessObservationUnavailable
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, ReasonProcessObservationUnavailable
	}
	uncertain := false
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false, ReasonProcessObservationUnavailable
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		_, err := os.Stat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, ReasonProcessObservationUnavailable
		}
		info, err := os.Stat(filepath.Join(dir, "exe"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Continue looking for a positive identity match even when another
			// process is hidden. Hidden entries cannot prove absence at the end.
			uncertain = true
			continue
		}
		if os.SameFile(want, info) {
			return true, nil
		}
	}
	if uncertain {
		return false, ReasonProcessObservationUnavailable
	}
	return false, nil
}
