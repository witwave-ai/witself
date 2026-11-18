//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const drillReservePlatformExtents = 0x80000 // FS_EXTENT_FL, linux/fs.h: the inode maps its blocks by extents

// drillReservePlatformFile is the row's own private file in its own directory, held read-write.
func drillReservePlatformFile(t *testing.T) (*os.File, string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "reserved")
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	drillAssert(t, err == nil, "cannot create the drill reserve fixture file")
	t.Cleanup(func() { _ = f.Close() })
	return f, name
}

// drillReservePlatformMap is the row's own FIEMAP call: one synced batch of at most 32 extents.
func drillReservePlatformMap(t *testing.T, f *os.File, length uint64) drillFiemap {
	t.Helper()
	m := drillFiemap{length: length, flags: 1, count: 32}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), drillFiemapIoctl, uintptr(unsafe.Pointer(&m)))
	drillAssert(t, errno == 0 && m.mapped <= 32, "the row's own extent map failed")
	return m
}

// TestDrillReservePlatformChild is efbig_child's child and does nothing unless its variable names the
// row's file: it lowers its own file size limit, allocates past it, restores the limit and prints the reason.
func TestDrillReservePlatformChild(_ *testing.T) {
	name := os.Getenv("WITSELF_DRILL_RESERVE_CHILD")
	if name == "" {
		return
	}
	reason, prior := "unopened", unix.Rlimit{}
	f, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err == nil && unix.Getrlimit(unix.RLIMIT_FSIZE, &prior) == nil {
		limit := prior
		limit.Cur, reason = 128<<10, "unlimited"
		if unix.Setrlimit(unix.RLIMIT_FSIZE, &limit) == nil {
			reason = "allocated"
			for offset := uint64(0); offset < 256<<10 && reason == "allocated"; offset += 64 << 10 {
				if err := drillPlatformAllocate(f, offset, 64<<10); err != nil {
					reason = drillReserveReason(err, "other")
				}
			}
			if unix.Setrlimit(unix.RLIMIT_FSIZE, &prior) != nil {
				reason = "unrestored"
			}
		}
	}
	if err == nil {
		_ = f.Close()
	}
	fmt.Println("drill-platform " + reason)
}

// The Linux platform layer on the runner's ext4: allocation, its proof and the private creators. A row
// allocates at most 1 MiB; lack of space comes only from the size-limited child.
func TestDrillReservePlatformLinux(t *testing.T) {
	t.Run("allocated", func(t *testing.T) {
		f, name := drillReservePlatformFile(t)
		var err error
		for offset := uint64(0); offset < 192<<10; offset += 64 << 10 {
			err = errors.Join(err, drillPlatformAllocate(f, offset, 64<<10))
		}
		reopened, openErr := os.Open(name)
		drillAssert(t, errors.Join(err, f.Sync(), f.Close(), openErr) == nil, "cannot allocate the drill reserve fixture")
		defer func() { _ = reopened.Close() }()
		drillAssert(t, drillPlatformVerify(reopened, 192<<10) == nil, "an allocated file was not proved on its reopened handle")
	})
	t.Run("in_place", func(t *testing.T) {
		f, name := drillReservePlatformFile(t)
		drillAssert(t, drillPlatformAllocate(f, 0, 1<<20) == nil && f.Sync() == nil, "cannot allocate the drill reserve fixture")
		before := drillReservePlatformMap(t, f, 1<<20)
		writer, err := os.OpenFile(name, os.O_WRONLY, 0)
		drillAssert(t, err == nil, "cannot open the drill reserve fixture for writing")
		_, err = writer.Write(bytes.Repeat([]byte{0x5a}, 1<<20))
		drillAssert(t, errors.Join(err, writer.Sync(), writer.Close()) == nil, "cannot write the drill reserve fixture")
		after := drillReservePlatformMap(t, f, 1<<20)
		same := before.mapped > 0 && before.mapped == after.mapped
		for index := range before.mapped {
			was, now := before.extents[index], after.extents[index]
			same = same && was.flags&drillExtentUnwritten != 0 && now.flags&drillExtentUnwritten == 0 && was.physical == now.physical && was.length == now.length
		}
		drillAssert(t, same && drillPlatformVerify(f, 1<<20) == nil, "a full write did not land in the allocated extents")
	})
	t.Run("size_differs", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		drillAssert(t, drillPlatformAllocate(f, 0, 8192) == nil && drillPlatformVerify(f, 8192) == nil, "cannot prove the drill reserve fixture")
		drillStorageReason(t, drillPlatformVerify(f, 4096), "allocation_unproved", "a file longer than its size was proved")
	})
	t.Run("hole_within", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		var stat unix.Stat_t
		fd := int(f.Fd())
		err := errors.Join(unix.Fallocate(fd, 0, 4096, 4096), unix.Fallocate(fd, unix.FALLOC_FL_KEEP_SIZE, 8192, 8192), unix.Fstat(fd, &stat))
		drillAssert(t, err == nil && stat.Size == 8192 && stat.Blocks*512 >= 8192, "cannot prepare the drill reserve hole")
		drillStorageReason(t, drillPlatformVerify(f, 8192), "allocation_unproved", "a file with a hole was proved")
	})
	t.Run("unsynced", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		_, err := f.Write(bytes.Repeat([]byte{0x5a}, 64<<10))
		drillAssert(t, err == nil && drillPlatformVerify(f, 64<<10) == nil, "a buffered write was not proved through the synced map")
	})
	t.Run("many_extents", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		var err error
		for block := int64(0); block < 66; block++ {
			if block%2 == 0 {
				_, written := f.WriteAt(bytes.Repeat([]byte{0x5a}, 4096), block*4096)
				err = errors.Join(err, written)
			} else {
				err = errors.Join(err, drillPlatformAllocate(f, uint64(block)*4096, 4096))
			}
		}
		drillAssert(t, errors.Join(err, f.Sync()) == nil && drillReservePlatformMap(t, f, 66*4096).mapped == 32, "cannot prepare more extents than one map holds")
		drillAssert(t, drillPlatformVerify(f, 66*4096) == nil, "a file of more extents than one map was not proved")
	})
	t.Run("sparse", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		drillAssert(t, f.Truncate(64<<10) == nil, "cannot prepare the sparse drill reserve fixture")
		drillStorageReason(t, drillPlatformVerify(f, 64<<10), "allocation_unproved", "a sparse file was proved")
	})
	t.Run("no_extents", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		fd := int(f.Fd())
		flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		drillAssert(t, err == nil && flags&drillReservePlatformExtents != 0, "the drill reserve fixture maps no extents")
		drillAssert(t, unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags&^drillReservePlatformExtents)) == nil, "cannot clear the fixture's extents flag")
		reason := drillReserveReason(drillPlatformAllocate(f, 0, 4096), "x")
		info, err := f.Stat()
		drillAssert(t, reason == "unsupported_fs" && err == nil && info.Size() == 0, "a file without extents was not refused as unsupported")
	})
	t.Run("efbig_child", func(t *testing.T) {
		f, name := drillReservePlatformFile(t)
		cover, args := t.TempDir(), []string{"-test.run=^TestDrillReservePlatformChild$", "-test.count=1"}
		if testing.CoverMode() != "" {
			args = append(args, "-test.gocoverdir="+cover)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		child := exec.CommandContext(ctx, os.Args[0], args...)
		child.Env = []string{"GOCOVERDIR=" + cover, "WITSELF_DRILL_RESERVE_CHILD=" + name}
		output, err := child.Output()
		var lines []string
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "drill-") {
				lines = append(lines, line)
			}
		}
		drillAssert(t, err == nil && slices.Equal(lines, []string{"drill-platform storage_exhausted"}), "the size-limited child did not exit 0 refused as storage_exhausted")
		var stat unix.Stat_t
		drillAssert(t, unix.Fstat(int(f.Fd()), &stat) == nil && stat.Size == 128<<10 && stat.Blocks*512 <= 128<<10+4096, "the size-limited child's file holds more than its limit")
	})
	t.Run("collision", func(t *testing.T) {
		const drillID = "drill_aaaaaaaaaaaaaaaa"
		v := newCheckpointVerifyCase(t, false)
		before := checkpointTreeState(t, v.c)
		boundary, err := os.OpenRoot(v.c.recoveryDir() + drillRootHere)
		drillAssert(t, err == nil, "cannot hold the drill reserve fixture boundary")
		defer func() { _ = boundary.Close() }()
		drillAssert(t, drillPlatformMkdir(v.c, boundary, nil, drillID, "") == nil, "the drill directory was not created")
		drill, err := boundary.OpenRoot(drillID)
		drillAssert(t, err == nil, "cannot hold the created drill directory")
		defer func() { _ = drill.Close() }()
		f, err := drillPlatformCreate(v.c, boundary, drill, drillID, "clone")
		drillAssert(t, err == nil && f.Close() == nil && drillPlatformMkdir(v.c, boundary, drill, drillID, "sub") == nil, "the drill's file or directory was not created")
		_, again := drillPlatformCreate(v.c, boundary, drill, drillID, "clone")
		for _, err := range []error{again, drillPlatformMkdir(v.c, boundary, nil, drillID, ""), drillPlatformMkdir(v.c, boundary, drill, drillID, "sub")} {
			drillAssert(t, drillReserveReason(err, "x") == "allocation_collision", "a repeated creation was not refused as a collision")
		}
		file, fileErr := drill.Lstat("clone")
		dir, dirErr := boundary.Lstat(drillID)
		sub, subErr := drill.Lstat("sub")
		drillAssert(t, errors.Join(fileErr, dirErr, subErr) == nil && file.Mode() == 0o600 && dir.Mode() == fs.ModeDir|0o700 && sub.Mode() == fs.ModeDir|0o700 &&
			v.c.validateRecovery() == nil, "the created drill tree is not private")
		// The row removes its own drill: the cell is left as it was.
		drillAssert(t, drill.Close() == nil && os.RemoveAll(filepath.Join(v.c.recoveryDir(), drillID)) == nil &&
			reflect.DeepEqual(before, checkpointTreeState(t, v.c)), "the row's drill directory was not removed")
	})
	t.Run("layout", func(t *testing.T) {
		var m drillFiemap
		drillAssert(t, unsafe.Offsetof(m.extents) == 32 && unsafe.Sizeof(m.extents[0]) == 56 && unsafe.Sizeof(m) == 32+32*56, "the extent map differs from struct fiemap")
	})
	t.Run("refusal", func(t *testing.T) {
		for err, want := range map[error]string{unix.ENOSPC: "storage_exhausted", unix.EDQUOT: "storage_exhausted", unix.EFBIG: "storage_exhausted", unix.EOPNOTSUPP: "unsupported_fs", unix.EIO: ""} {
			mapped := map[bool]string{true: want, false: "x"}[want != ""]
			drillAssert(t, drillPlatformRefusal(err) == want && drillPlatformRefusal(&os.SyscallError{Syscall: "fallocate", Err: err}) == want &&
				drillReserveReason(err, "x") == mapped, "the platform refusal misjudged an error")
		}
	})
}
