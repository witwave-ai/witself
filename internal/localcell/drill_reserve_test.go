package localcell

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"syscall"
	"testing"
)

// The reservation's pure decisions, on every system.
func TestDrillReservePlatform(t *testing.T) {
	t.Run("extents", func(t *testing.T) {
		const last, unwritten = drillExtentLast, drillExtentUnwritten
		for index, row := range []struct {
			extents []drillExtent
			size    uint64
			want    bool
		}{
			{nil, 0, true},
			{nil, 1, false},
			{[]drillExtent{{0, 4096, last | unwritten}}, 4096, true},
			{[]drillExtent{{0, 4096, 0}, {4096, 4096, last}}, 8192, true},
			{[]drillExtent{{0, 4096, 0}, {8192, 4096, last}}, 12288, false},
			{[]drillExtent{{4096, 8192, last}}, 8192, false},
			{[]drillExtent{{0, 4096, 0x2000 | last}}, 4096, false},
			{[]drillExtent{{0, 4096, 0x4}}, 4096, false},
			{[]drillExtent{{0, 4096, 0x7}}, 4096, false},
			{[]drillExtent{{0, 4096, 0x200 | last}}, 4096, false},
			{[]drillExtent{{0, 4096, 0x8 | last}}, 4096, false},
			{[]drillExtent{{0, 4096, 0x2}}, 4096, false},           // unknown alone
			{[]drillExtent{{0, 4096, 0x80 | last}}, 4096, false},   // data encrypted
			{[]drillExtent{{0, 4096, 0x100 | last}}, 4096, false},  // not aligned
			{[]drillExtent{{0, 4096, 0x400 | last}}, 4096, false},  // data tail
			{[]drillExtent{{0, 4096, 0x1000 | last}}, 4096, false}, // merged
			{[]drillExtent{{0, 0, last}}, 4096, false},
			{[]drillExtent{{0, 2048, unwritten | last}}, 4096, false},
			{[]drillExtent{{0, 4096, 0}, {4096, 4096, 0x2000 | last}}, 4096, true},
			{[]drillExtent{{0, math.MaxUint64, last}}, math.MaxUint64, true},
			{[]drillExtent{{0, 4096, 0}, {8192, 8192, last}}, 12288, false},
			{[]drillExtent{{0, 8192, 0}, {4096, 8192, last}}, 12288, false},
		} {
			if drillExtentsCover(row.extents, row.size) != row.want {
				t.Fatalf("the extent decision misjudged map %d", index)
			}
		}
	})
	t.Run("reason", func(t *testing.T) {
		drillAssert(t, drillReserveReason(drillStorageError{reason: "cancelled"}, "x") == "cancelled" &&
			drillReserveReason(fs.ErrExist, "x") == "allocation_collision" &&
			drillReserveReason(fmt.Errorf("drill reserve fixture: %w", fs.ErrExist), "x") == "allocation_collision" &&
			drillReserveReason(errors.New("other"), "fallback") == "fallback", "the reservation reason mapping misjudged an error")
		drillAssert(t, drillReserveReason(fs.ErrNotExist, "fallback") == "fallback" &&
			drillReserveReason(&fs.PathError{Op: "openat", Path: "x", Err: syscall.ENOENT}, "fallback") == "fallback", "the reservation reason mapping misjudged a missing name")
	})
}
