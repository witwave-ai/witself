//go:build linux

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// drillFiemapIoctl is FS_IOC_FIEMAP, _IOWR('f', 11, struct fiemap) in
// linux/fs.h, in the generic ioctl encoding (amd64, arm64); where the encoding
// differs the call fails and verification refuses.
const drillFiemapIoctl = 0xC020660B

// drillFiemap is struct fiemap with room for 32 struct fiemap_extent (linux/fiemap.h).
type drillFiemap struct {
	start, length                  uint64
	flags, mapped, count, reserved uint32
	extents                        [32]struct {
		logical, physical, length uint64
		reserved64                [2]uint64
		flags                     uint32
		reserved                  [3]uint32
	}
}

// drillPlatformMkdir makes a private directory through a held root: the drill
// directory in recovery/ when drill is nil, else name inside the drill.
func drillPlatformMkdir(_ *Cell, boundary, drill *os.Root, drillID, name string) error {
	if drill == nil {
		return boundary.Mkdir(drillID, 0o700)
	}
	return drill.Mkdir(filepath.FromSlash(name), 0o700)
}

// drillPlatformCreate creates a private file exclusively through the held drill root.
func drillPlatformCreate(_ *Cell, _, drill *os.Root, _, name string) (*os.File, error) {
	return drill.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// drillPlatformAllocate allocates [offset, offset+length) with fallocate mode
// 0, which also extends the size; a failed call keeps what it allocated.
func drillPlatformAllocate(f *os.File, offset, length uint64) error {
	return unix.Fallocate(int(f.Fd()), 0, int64(offset), int64(length))
}

// drillPlatformRefusal: no space, a quota or the file size limit is
// storage_exhausted; a file without extents (ext2 or ext3 format) is unsupported_fs.
func drillPlatformRefusal(err error) string {
	switch {
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT), errors.Is(err, unix.EFBIG):
		return "storage_exhausted"
	case errors.Is(err, unix.EOPNOTSUPP):
		return "unsupported_fs"
	}
	return ""
}

// drillPlatformVerify proves [0, size) of f allocated: f is exactly size long,
// and a FIEMAP map taken with FIEMAP_FLAG_SYNC covers it with no gap.
func drillPlatformVerify(f *os.File, size uint64) error {
	refused := drillStorageError{reason: "allocation_unproved"}
	info, err := f.Stat()
	if err != nil || info.Size() < 0 || uint64(info.Size()) != size {
		return refused
	}
	fd := int(f.Fd())
	var extents []drillExtent
	for start := uint64(0); start < size; {
		m := drillFiemap{start: start, length: size - start, flags: 1, count: 32}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), drillFiemapIoctl, uintptr(unsafe.Pointer(&m))); errno != 0 || m.mapped == 0 || m.mapped > 32 {
			return refused
		}
		for _, extent := range m.extents[:m.mapped] {
			extents = append(extents, drillExtent{logical: extent.logical, length: extent.length, flags: extent.flags})
		}
		last := m.extents[m.mapped-1]
		next := drillStorageAdd(last.logical, last.length)
		if last.flags&drillExtentLast != 0 || next <= start {
			break
		}
		start = next
	}
	if !drillExtentsCover(extents, size) {
		return refused
	}
	return nil
}
