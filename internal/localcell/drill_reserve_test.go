package localcell

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
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
	t.Run("windows_decision", func(t *testing.T) {
		for index, row := range []struct {
			allocation, end int64
			attributes      uint32
			size            uint64
			want            bool
		}{
			{8192, 4096, 0, 4096, true}, {8192, 4096, 0x20 | 0x80, 4096, true}, {8192, 8192, 0, 4096, false}, {8192, 4096, 0, 8192, false},
			{4095, 4096, 0, 4096, false}, {8192, -1, 0, math.MaxUint64, false}, {8192, 4096, 0x10, 4096, false}, {8192, 4096, 0x200, 4096, false},
			{8192, 4096, 0x400, 4096, false}, {8192, 4096, 0x800, 4096, false}, {8192, 4096, 0x4000, 4096, false}, {8192, 4096, 0x8000, 4096, false},
		} {
			if drillWindowsProved(row.allocation, row.end, row.attributes, row.size) != row.want {
				t.Fatalf("the windows allocation decision misjudged case %d", index)
			}
		}
	})
	t.Run("paths_fit", func(t *testing.T) {
		base := string(filepath.Separator) + strings.Repeat("b", 150)
		for index, row := range []struct {
			base    string
			entries []checkpointInventoryEntry
			want    bool
		}{
			{base, []checkpointInventoryEntry{{Path: "pg/base", Type: "dir"}}, true}, {base, []checkpointInventoryEntry{{Path: "pg/" + strings.Repeat("c", 80), Type: "file"}}, false},
			{base, []checkpointInventoryEntry{{Path: "pg/" + strings.Repeat("c", 50), Type: "file"}}, true},
			{base, []checkpointInventoryEntry{{Path: "pg/" + strings.Repeat("c", 50), Type: "dir"}}, false}, {strings.Repeat("b", 230), nil, false},
		} {
			if drillReservePathsFit(row.base, row.entries) != row.want {
				t.Fatalf("the drill layout path bound misjudged case %d", index)
			}
		}
	})
}
