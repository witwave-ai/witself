package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	drillReserveClone    = "clone" // the clone's tree inside the drill directory
	drillExtentLast      = 0x1     // FIEMAP_EXTENT_LAST, linux/fiemap.h
	drillExtentUnwritten = 0x800   // FIEMAP_EXTENT_UNWRITTEN, linux/fiemap.h
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

// drillWindowsRefused are the attributes that keep an end-of-file allocation
// from proving its range: directory 0x10, sparse 0x200, reparse point 0x400,
// compressed 0x800, offline 0x1000, encrypted 0x4000, integrity stream 0x8000,
// recall on data access 0x400000 (a placeholder whose data is elsewhere).
const drillWindowsRefused = 0x10 | 0x200 | 0x400 | 0x800 | 0x1000 | 0x4000 | 0x8000 | 0x400000

// drillWindowsProved: a file exactly size long whose allocation covers its end
// and that carries no refused attribute.
func drillWindowsProved(allocation, end int64, attributes uint32, size uint64) bool {
	return end >= 0 && uint64(end) == size && allocation >= end && attributes&drillWindowsRefused == 0
}

// drillReservePathsFit applies the Windows path bound to the drill layout; the
// clone directory's bound covers the shorter record temporary and allowance.
func drillReservePathsFit(boundaryDir string, entries []checkpointInventoryEntry) bool {
	sample := filepath.Join(boundaryDir, "drill_"+strings.Repeat("a", 16))
	fits := checkpointPathFits(filepath.Join(sample, drillReserveClone), true)
	for _, entry := range entries {
		fits = fits && checkpointPathFits(filepath.Join(sample, drillReserveClone, filepath.FromSlash(entry.Path)), entry.Type == "dir")
	}
	return fits
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

// drillReserveSame reports whether f, still open, and info read through a
// name are one file: an identity compared while f holds the file cannot be
// matched by a freed and reused file number. A pathname FileInfo's identity is
// read at this comparison (Windows resolves it lazily), as the deletion's bind.
func drillReserveSame(f *os.File, info os.FileInfo, err error) bool {
	mine, statErr := f.Stat()
	return err == nil && statErr == nil && os.SameFile(mine, info)
}

// drillReserveRoot opens name below parent as a root and proves that the root
// is the directory object the no-follow walk reached at that name: the anchor,
// opened through the walk, and the root are compared while both are open. That
// object renamed between the two opens and reached again through an in-root
// link is accepted, being the same object; any other is boundary_refused, and
// so is the proved directory failing to open as a root (that error would carry
// the name). The anchor serves identity only; a parent renamed meanwhile is
// followed as its directory object.
func drillReserveRoot(parent *os.Root, name string) (*os.Root, error) {
	anchor, err := drillPlatformOpen(parent, name, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = anchor.Close() }()
	root, err := parent.OpenRoot(filepath.FromSlash(name) + drillRootHere)
	if err != nil {
		return nil, drillStorageError{reason: "boundary_refused"}
	}
	info, err := root.Stat(".")
	if !drillReserveSame(anchor, info, err) {
		_ = root.Close()
		return nil, drillStorageError{reason: "boundary_refused"}
	}
	return root, nil
}
