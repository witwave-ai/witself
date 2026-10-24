//go:build darwin

package localcell

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func processIdentity(pid int) (string, bool, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)>>1) {
		return "", false, errors.New("localcell: invalid process ID")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// Darwin returns an empty sysctl result (reported as EIO by x/sys)
		// for absent PIDs. Probe only this PID; never enumerate processes.
		if errors.Is(err, unix.ESRCH) || (errors.Is(err, unix.EIO) && errors.Is(unix.Kill(pid, 0), unix.ESRCH)) {
			return "", false, nil
		}
		return "", false, errors.New("localcell: cannot verify process creation time")
	}
	if int(info.Proc.P_pid) != pid || info.Proc.P_starttime.Sec <= 0 {
		return "", false, errors.New("localcell: invalid process creation identity")
	}
	// Darwin's SZOMB state is 5. Zombies have exited and cannot serve traffic.
	if info.Proc.P_stat == 5 {
		return "", false, nil
	}
	return fmt.Sprintf("darwin:%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), true, nil
}

func processStartedAt(pid int) (time.Time, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)>>1) {
		return time.Time{}, errors.New("localcell: invalid process ID")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(info.Proc.P_pid) != pid || info.Proc.P_starttime.Sec <= 0 || info.Proc.P_stat == 5 {
		return time.Time{}, errors.New("localcell: cannot verify process creation time")
	}
	return time.Unix(info.Proc.P_starttime.Sec, int64(info.Proc.P_starttime.Usec)*1000).UTC(), nil
}
