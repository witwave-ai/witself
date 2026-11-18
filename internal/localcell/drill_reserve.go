package localcell

import (
	"errors"
	"io/fs"
)

const (
	drillExtentLast      = 0x1   // FIEMAP_EXTENT_LAST, linux/fiemap.h
	drillExtentUnwritten = 0x800 // FIEMAP_EXTENT_UNWRITTEN, linux/fiemap.h
)

// drillExtent is one mapped range of a file: its logical start, length and flags.
type drillExtent struct {
	logical, length uint64
	flags           uint32
}

// drillExtentsCover reports whether extents, in logical order, cover [0, size)
// with no gap and no flag but last and unwritten.
func drillExtentsCover(extents []drillExtent, size uint64) bool {
	next := uint64(0)
	for _, extent := range extents {
		if next >= size {
			break
		}
		if extent.logical != next || extent.flags&^(drillExtentLast|drillExtentUnwritten) != 0 {
			return false
		}
		next = drillStorageAdd(next, extent.length)
	}
	return next >= size
}

// drillReserveReason is the closed reason a create, write or allocation error
// carries: a closed refusal as itself, an existing name, the platform's lack of
// space or of support, else otherwise.
func drillReserveReason(err error, otherwise string) string {
	var refused drillStorageError
	switch platform := drillPlatformRefusal(err); {
	case errors.As(err, &refused):
		return refused.reason
	case errors.Is(err, fs.ErrExist):
		return "allocation_collision"
	case platform != "":
		return platform
	}
	return otherwise
}
