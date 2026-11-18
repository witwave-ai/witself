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

// drillReservePlatformFile is the row's own private file in its own directory, held read-write. A row that
// allocates or proves needs that directory on ext4 and fails first, never skips, anywhere else.
func drillReservePlatformFile(t *testing.T) (*os.File, string) {
	t.Helper()
	dir, volume := t.TempDir(), unix.Statfs_t{}
	drillAssert(t, unix.Statfs(dir, &volume) == nil && volume.Type == unix.EXT4_SUPER_MAGIC, "the reservation rows need an ext4 temporary directory")
	name := filepath.Join(dir, "reserved")
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

// drillReserveHeld holds the directory at name as a root until the row ends.
func drillReserveHeld(t *testing.T, name string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(name)
	drillAssert(t, err == nil, "cannot hold the drill reserve fixture root")
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// drillReserveTree is the type of every name below top, links unfollowed.
func drillReserveTree(t *testing.T, top string) map[string]fs.FileMode {
	t.Helper()
	tree := map[string]fs.FileMode{}
	err := filepath.WalkDir(top, func(name string, entry fs.DirEntry, err error) error {
		if err == nil {
			tree[name] = entry.Type()
		}
		return err
	})
	drillAssert(t, err == nil, "cannot read the drill reserve fixture tree")
	return tree
}

// drillReserveRefused reports err a bare errno, which carries no name, whose closed reason is want,
// "fallback" being the caller's own.
func drillReserveRefused(err error, want string) bool {
	errno, bare := err.(unix.Errno)
	return bare && errno != 0 && drillReserveReason(err, "fallback") == want
}

// drillReserveWatched creates the file file and makes the directory dir through drill, never waiting: a
// creation still running after ten seconds fails the row, and the FIFO at fifo is opened read-write until
// both return, so nothing is left blocked.
func drillReserveWatched(t *testing.T, drill *os.Root, fifo, dir, file string) (error, error) {
	t.Helper()
	done, waited := make(chan [2]error, 1), false
	go func() {
		created, fileErr := drillPlatformCreate(nil, nil, drill, "", file)
		if created != nil {
			_ = created.Close()
		}
		done <- [2]error{drillPlatformMkdir(nil, nil, drill, "", dir), fileErr}
	}()
	for {
		select {
		case errs := <-done:
			drillAssert(t, !waited, "a drill reserve creation waited at a FIFO")
			return errs[0], errs[1]
		case <-time.After(10 * time.Second):
			if f, err := os.OpenFile(fifo, os.O_RDWR|unix.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			waited = true
		}
	}
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
	t.Run("shorter", func(t *testing.T) {
		f, _ := drillReservePlatformFile(t)
		fd := int(f.Fd())
		drillAssert(t, unix.Fallocate(fd, 0, 0, 4096) == nil && unix.Fallocate(fd, unix.FALLOC_FL_KEEP_SIZE, 4096, 4096) == nil, "cannot prepare the shorter drill reserve fixture")
		drillStorageReason(t, drillPlatformVerify(f, 8192), "allocation_unproved", "a file shorter than its size was proved")
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
		_, below := drillPlatformCreate(v.c, boundary, drill, drillID, "absent/clone")
		drillAssert(t, drillReserveRefused(below, "fallback"), "a create below an absent directory was not refused with the fallback")
		file, fileErr := drill.Lstat("clone")
		dir, dirErr := boundary.Lstat(drillID)
		sub, subErr := drill.Lstat("sub")
		drillAssert(t, errors.Join(fileErr, dirErr, subErr) == nil && file.Mode() == 0o600 && dir.Mode() == fs.ModeDir|0o700 && sub.Mode() == fs.ModeDir|0o700 &&
			v.c.validateRecovery() == nil, "the created drill tree is not private")
		// The row removes its own drill: the cell is left as it was.
		drillAssert(t, drill.Close() == nil && os.RemoveAll(filepath.Join(v.c.recoveryDir(), drillID)) == nil &&
			reflect.DeepEqual(before, checkpointTreeState(t, v.c)), "the row's drill directory was not removed")
	})
	// A link, a regular file or a FIFO before the last component is refused without waiting, and an existing
	// name at the last is a collision; nothing is created anywhere, in the link's target least of all.
	for _, row := range []struct{ name, plant, dir, file, want string }{
		{"link_component", "inside", "a/x", "a/y", "fallback"}, {"link_dangling", "dangling", "a/x", "a/y", "fallback"},
		{"link_outside", "outside", "a/x", "a/y", "fallback"}, {"file_component", "file", "a/x", "a/y", "fallback"},
		{"fifo_component", "fifo", "a/x", "a/y", "fallback"}, {"fifo_last", "fifo", "a", "a", "allocation_collision"},
		{"dangling_last", "dangling", "a", "a", "allocation_collision"},
	} {
		t.Run(row.name, func(t *testing.T) {
			top := t.TempDir()
			drillDir, outside, planted := filepath.Join(top, "drill"), filepath.Join(top, "outside"), filepath.Join(top, "drill", "a")
			err := errors.Join(os.MkdirAll(filepath.Join(drillDir, "b"), 0o700), os.Mkdir(outside, 0o700))
			switch row.plant {
			case "inside":
				err = errors.Join(err, os.Symlink("b", planted))
			case "dangling":
				err = errors.Join(err, os.Symlink("missing", planted))
			case "outside":
				err = errors.Join(err, os.Symlink(outside, planted))
			case "file":
				err = errors.Join(err, os.WriteFile(planted, nil, 0o600))
			default:
				err = errors.Join(err, unix.Mkfifo(planted, 0o600))
			}
			drillAssert(t, err == nil, "cannot plant the drill reserve fixture component")
			drill, before := drillReserveHeld(t, drillDir), drillReserveTree(t, top)
			dirErr, fileErr := drillReserveWatched(t, drill, planted, row.dir, row.file)
			drillAssert(t, drillReserveRefused(dirErr, row.want) && drillReserveRefused(fileErr, row.want) && reflect.DeepEqual(before, drillReserveTree(t, top)),
				"a creation went through a planted component or past its refusal")
		})
	}
	t.Run("malformed", func(t *testing.T) {
		top := t.TempDir()
		drillAssert(t, os.MkdirAll(filepath.Join(top, "b", "a"), 0o700) == nil, "cannot prepare the drill reserve fixture tree")
		live, closed, before := drillReserveHeld(t, top), drillReserveHeld(t, top), drillReserveTree(t, top)
		// A closed root refuses EBADF once opened: EINVAL from it shows no system call was reached.
		drillAssert(t, closed.Close() == nil, "cannot close the drill reserve fixture root")
		for _, root := range []*os.Root{live, closed} {
			for _, name := range []string{"", "/x", "x/", "a//x", "./x", "b/./x", ".", "b/../x", "b/a/..", "..", "../x"} {
				_, fileErr := drillPlatformCreate(nil, nil, root, "", name)
				drillAssert(t, drillPlatformMkdir(nil, nil, root, "", name) == unix.EINVAL && fileErr == unix.EINVAL, "a malformed name was not refused before any system call")
			}
			for _, drillID := range []string{"b/x", "", ".", "..", "/x", "x/"} {
				drillAssert(t, drillPlatformMkdir(nil, root, nil, drillID, "") == unix.EINVAL, "an identifier of other than one plain component was not refused")
			}
		}
		drillAssert(t, drillPlatformMkdir(nil, nil, closed, "", "x") == unix.EBADF, "a closed root's refusal was not a bare errno")
		drillAssert(t, reflect.DeepEqual(before, drillReserveTree(t, top)), "a malformed name or identifier created something")
	})
	t.Run("device", func(t *testing.T) {
		drillAssert(t, drillPlatformDevice(7, 7) == nil && drillPlatformDevice(7, 8) == unix.EXDEV && drillReserveRefused(drillPlatformDevice(7, 8), "fallback"),
			"the device comparison misjudged two devices")
		// No mount can be made here, but proc is one below the system root everywhere: the walk refuses it.
		slash := drillReserveHeld(t, "/")
		_, fileErr := drillPlatformCreate(nil, nil, slash, "", "proc/drill_reserve")
		drillAssert(t, drillPlatformMkdir(nil, nil, slash, "", "proc/drill_reserve") == unix.EXDEV && fileErr == unix.EXDEV, "a creation crossed onto another device")
	})
	t.Run("descriptors", func(t *testing.T) {
		top := t.TempDir()
		drillAssert(t, os.MkdirAll(filepath.Join(top, "d", "kept"), 0o700) == nil && os.Symlink("kept", filepath.Join(top, "d", "link")) == nil, "cannot prepare the drill reserve fixture tree")
		drill, slash := drillReserveHeld(t, top), drillReserveHeld(t, "/")
		top, err := filepath.EvalSymlinks(top)
		drillAssert(t, err == nil, "cannot resolve the drill reserve fixture tree")
		// Each round makes four (two of three components) and refuses four: a link and a collision after an
		// opened directory, another device, a malformed name. Only descriptors on the fixture, the system root
		// or proc are counted, so another test's file closed by the collector cannot disturb the comparison.
		rounds := func(from, to int) (open int) {
			for round := from; round < to; round++ {
				suffix := fmt.Sprint(round)
				made, madeErr := drillPlatformCreate(nil, nil, drill, "", "d/kept/f"+suffix)
				file, fileErr := drillPlatformCreate(nil, nil, drill, "", "d/f"+suffix)
				_, existing := drillPlatformCreate(nil, nil, drill, "", "d/kept")
				_, malformed := drillPlatformCreate(nil, nil, drill, "", "d//f")
				drillAssert(t, errors.Join(madeErr, fileErr, made.Close(), file.Close(), drillPlatformMkdir(nil, nil, drill, "", "d/m"+suffix),
					drillPlatformMkdir(nil, nil, drill, "", "d/kept/m"+suffix)) == nil && existing != nil && malformed != nil &&
					drillPlatformMkdir(nil, nil, drill, "", "d/link/m") != nil && drillPlatformMkdir(nil, nil, slash, "", "proc/m") != nil, "a creation of the descriptor rounds misbehaved")
			}
			entries, err := os.ReadDir("/proc/self/fd")
			drillAssert(t, err == nil, "cannot list the row's open descriptors")
			for _, entry := range entries {
				if target, _ := os.Readlink("/proc/self/fd/" + entry.Name()); target == "/" || target == "/proc" || strings.HasPrefix(target+"/", top+"/") {
					open++
				}
			}
			return open
		}
		before := rounds(0, 0) // the two held roots at least
		drillAssert(t, before >= 2 && before == rounds(0, 50), "the creators leaked a descriptor over the refused and the made rounds")
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
