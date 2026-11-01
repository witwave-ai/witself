//go:build linux

package localcell

import (
	"encoding/binary"
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestLinuxProcessStatReadAfterExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		gone bool
	}{
		{"missing before open", syscall.ENOENT, true},
		{"exited during read", syscall.ESRCH, true},
		{"permission denied", syscall.EACCES, false},
		{"read failed", syscall.EIO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			read := func(path string) ([]byte, error) {
				reads++
				if path != "/proc/42/stat" {
					t.Fatal("process stat observation selected the wrong process")
				}
				return nil, &os.PathError{Op: "read", Path: path, Err: tc.err}
			}
			start, alive, err := linuxProcessStartTicksWithRead(42, read)
			if reads != 1 {
				t.Fatal("process stat observation retried a failed identity read")
			}
			if start != 0 || alive {
				t.Fatal("failed process stat read fabricated a live identity")
			}
			if (err == nil) != tc.gone {
				t.Fatal("process stat exit read lost the inspection distinction")
			}
		})
	}
}

func TestLinuxClockTicksUsesKernelAuxiliaryVector(t *testing.T) {
	wordBytes := strconv.IntSize / 8
	vector := func(key, value uint64) []byte {
		data := make([]byte, 4*wordBytes)
		if wordBytes == 4 {
			binary.NativeEndian.PutUint32(data, uint32(key))
			binary.NativeEndian.PutUint32(data[wordBytes:], uint32(value))
		} else {
			binary.NativeEndian.PutUint64(data, key)
			binary.NativeEndian.PutUint64(data[wordBytes:], value)
		}
		return data
	}
	for _, frequency := range []uint64{100, 250, 1000} {
		ticks, err := linuxClockTicks(vector(17, frequency))
		if err != nil || ticks != frequency {
			t.Fatalf("kernel clock frequency %d was not retained", frequency)
		}
	}
	for _, data := range [][]byte{nil, {1}, vector(17, 0), vector(17, 1_000_001), vector(16, 100), vector(0, 100)} {
		if _, err := linuxClockTicks(data); err == nil {
			t.Fatal("invalid or missing kernel clock frequency was accepted")
		}
	}
}
