//go:build linux

package localcell

import (
	"encoding/binary"
	"strconv"
	"testing"
)

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
