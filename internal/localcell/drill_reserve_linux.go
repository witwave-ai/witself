//go:build linux

package localcell

import (
	"errors"
	"math"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// drillFiemapIoctl is FS_IOC_FIEMAP, _IOWR('f', 11, struct fiemap) in
// linux/fs.h; a read-write request encodes as 0xC0000000 in the generic and in
// the powerpc and mips layouts alike, so the value holds on every Linux GOARCH.
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

// drillPlatformDevice refuses a directory on another device than the held
// root's: a mount inside the tree.
func drillPlatformDevice(root, dir uint64) error {
	if dir != root {
		return unix.EXDEV
	}
	return nil
}

// drillPlatformAt runs create in the directory holding name's last component,
// reached from the held root's own descriptor one component at a time, each
// opened as a directory without following a link and required on the root's
// device. A name with an empty, "." or ".." component is refused before any
// system call. Every descriptor is closed; an error is a bare errno, no name.
func drillPlatformAt(root *os.Root, name string, create func(dir int, last string) error) error {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return unix.EINVAL
		}
	}
	held, err := root.Open(".")
	if errno := unix.EBADF; err != nil {
		_ = errors.As(err, &errno) // the system error alone; EBADF for a closed root
		return errno
	}
	defer func() { _ = held.Close() }()
	var device, stat unix.Stat_t
	dir := int(held.Fd())
	if err := unix.Fstat(dir, &device); err != nil {
		return err
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(next) }()
		if err := unix.Fstat(next, &stat); err != nil {
			return err
		}
		if err := drillPlatformDevice(uint64(device.Dev), uint64(stat.Dev)); err != nil {
			return err
		}
		dir = next
	}
	return create(dir, parts[len(parts)-1])
}

// drillPlatformMkdir makes a private directory through a held root: the drill
// directory, exactly one plain component, in recovery/ when drill is nil, else
// name inside the drill.
func drillPlatformMkdir(_ *Cell, boundary, drill *os.Root, drillID, name string) error {
	root := drill
	if drill == nil {
		if strings.Contains(drillID, "/") {
			return unix.EINVAL
		}
		root, name = boundary, drillID
	}
	return drillPlatformAt(root, name, func(dir int, last string) error { return unix.Mkdirat(dir, last, 0o700) })
}

// drillPlatformCreate creates a private file exclusively through the held drill
// root; the file's name is fixed, so no later error of it carries a path.
func drillPlatformCreate(_ *Cell, _, drill *os.Root, _, name string) (*os.File, error) {
	var created *os.File
	err := drillPlatformAt(drill, name, func(dir int, last string) error {
		fd, err := unix.Openat(dir, last, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			created = os.NewFile(uintptr(fd), "drill reserve file")
		}
		return err
	})
	return created, err
}

// drillPlatformOpen opens an existing directory or regular file for reading
// through the same walk: the last component is not followed either, a file is
// opened without blocking, so a planted FIFO is never waited on but refused,
// and never as a controlling terminal, and the object must be on its parent's
// device. The error is a bare errno.
func drillPlatformOpen(root *os.Root, name string, directory bool) (*os.File, error) {
	kind := 0
	if directory {
		kind = unix.O_DIRECTORY
	}
	var opened *os.File
	err := drillPlatformAt(root, name, func(dir int, last string) error {
		fd, err := unix.Openat(dir, last, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC|kind, 0)
		if err != nil {
			return err
		}
		var parent, stat unix.Stat_t
		if err = unix.Fstat(dir, &parent); err == nil {
			err = unix.Fstat(fd, &stat)
		}
		if err == nil {
			err = drillPlatformDevice(uint64(parent.Dev), uint64(stat.Dev))
		}
		if err == nil && !directory && stat.Mode&unix.S_IFMT != unix.S_IFREG {
			err = unix.EINVAL // a FIFO or another special file, opened without waiting
		}
		if err != nil {
			_ = unix.Close(fd)
			return err
		}
		opened = os.NewFile(uintptr(fd), "drill reserve file")
		return nil
	})
	return opened, err
}

// drillPlatformAllocate allocates [offset, offset+length) with fallocate mode
// 0, which extends the size when the range passes the end of the file; a
// failed call keeps what it allocated. Callers allocate in increasing order;
// mode 0 never shrinks the file. A range ending past the largest file offset
// is storage_exhausted before any system call, as on Windows.
func drillPlatformAllocate(f *os.File, offset, length uint64) error {
	if length > math.MaxInt64 || offset > math.MaxInt64-length {
		return drillStorageError{reason: "storage_exhausted"}
	}
	return unix.Fallocate(int(f.Fd()), 0, int64(offset), int64(length))
}

// drillPlatformInherits: on ext4 no attribute a new file inherits from its
// directory keeps the proof from mapping its extents.
func drillPlatformInherits(*os.File) bool { return false }

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

// drillPlatformVerify proves [0, size) of f allocated by the file system: f is
// exactly size long, and a FIEMAP map taken with FIEMAP_FLAG_SYNC covers it
// with no gap and only unwritten or written extents. It reads the file
// system's own map and makes no claim about the device below it.
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
