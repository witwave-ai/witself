//go:build linux

package localcell

import (
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processIdentity(pid int) (string, bool, error) {
	start, alive, err := linuxProcessStartTicks(pid)
	if err != nil || !alive {
		return "", alive, err
	}
	// Include the boot identity so a persistent record cannot match a reused
	// PID with the same tick count after a reboot.
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return "", false, errors.New("localcell: cannot verify process boot identity")
	}
	return "linux:" + strings.TrimSpace(string(boot)) + ":" + strconv.FormatUint(start, 10), true, nil
}

func linuxProcessStartTicks(pid int) (uint64, bool, error) {
	return linuxProcessStartTicksWithRead(pid, os.ReadFile)
}

func linuxProcessStartTicksWithRead(pid int, readFile func(string) ([]byte, error)) (uint64, bool, error) {
	if pid <= 0 {
		return 0, false, errors.New("localcell: invalid process ID")
	}
	data, err := readFile("/proc/" + strconv.Itoa(pid) + "/stat")
	// A process can disappear after procfs opens stat but before the read.
	// Linux then reports ESRCH rather than ENOENT. Both establish an exited
	// process; other read errors remain inspection failures, without retrying
	// or substituting a different process identity.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errors.New("localcell: cannot verify process creation time")
	}
	// comm is parenthesized and may contain spaces and closing parentheses.
	// The last ')' precedes fields 3..52; starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, false, errors.New("localcell: invalid process creation identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, false, errors.New("localcell: invalid process creation identity")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, false, nil
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, false, errors.New("localcell: invalid process creation identity")
	}
	return start, true, nil
}

func processStartedAt(pid int) (time.Time, error) {
	start, alive, err := linuxProcessStartTicks(pid)
	if err != nil || !alive {
		return time.Time{}, errors.New("localcell: cannot verify process creation time")
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, errors.New("localcell: cannot verify process boot time")
	}
	var boot int64
	for line := range strings.SplitSeq(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return time.Time{}, errors.New("localcell: invalid process boot time")
			}
			break
		}
	}
	if boot <= 0 {
		return time.Time{}, errors.New("localcell: missing process boot time")
	}
	// AT_CLKTCK supplies the kernel's actual user-space clock tick frequency.
	// Do not assume 100 Hz: creation timestamps are a recovery identity fence.
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return time.Time{}, errors.New("localcell: cannot verify process clock frequency")
	}
	ticks, err := linuxClockTicks(auxv)
	if err != nil || start/ticks > uint64(1<<63-1-boot) {
		return time.Time{}, errors.New("localcell: invalid process clock frequency")
	}
	return time.Unix(boot+int64(start/ticks), int64(start%ticks)*int64(time.Second)/int64(ticks)).UTC(), nil
}

func linuxClockTicks(auxv []byte) (uint64, error) {
	wordBytes := strconv.IntSize / 8
	if len(auxv)%(2*wordBytes) != 0 {
		return 0, errors.New("localcell: invalid process auxiliary vector")
	}
	readWord := func(value []byte) uint64 {
		if wordBytes == 4 {
			return uint64(binary.NativeEndian.Uint32(value))
		}
		return binary.NativeEndian.Uint64(value)
	}
	for offset := 0; offset+2*wordBytes <= len(auxv); offset += 2 * wordBytes {
		key, value := readWord(auxv[offset:]), readWord(auxv[offset+wordBytes:])
		if key == 0 {
			break
		}
		if key == 17 && value > 0 && value <= 1_000_000 { // AT_CLKTCK
			return value, nil
		}
	}
	return 0, errors.New("localcell: missing process clock frequency")
}
