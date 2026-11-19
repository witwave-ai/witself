//go:build windows

package localcell

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// drillReserveWindowsStandard is FILE_STANDARD_INFO (winbase.h), the production layout read by the rows.
type drillReserveWindowsStandard struct {
	allocation, end          int64
	links                    uint32
	deletePending, directory bool
}

// drillReserveWindowsClass names err by a fixed text per known value, never by its own text.
func drillReserveWindowsClass(err error) string {
	var refused drillStorageError
	switch {
	case err == nil:
		return "no error"
	case errors.As(err, &refused):
		return "the reason " + refused.reason
	}
	for errno, name := range map[syscall.Errno]string{windows.ERROR_FILE_EXISTS: "file exists", windows.ERROR_FILE_NOT_FOUND: "file not found",
		windows.ERROR_PATH_NOT_FOUND: "path not found", windows.ERROR_DIRECTORY: "not a directory", windows.ERROR_ACCESS_DENIED: "access denied",
		windows.ERROR_SHARING_VIOLATION: "sharing violation", windows.ERROR_INVALID_NAME: "invalid name", windows.ERROR_INVALID_HANDLE: "invalid handle",
		windows.ERROR_BAD_LENGTH: "bad length"} {
		if errors.Is(err, errno) { // an error wraps one system error at most
			return name
		}
	}
	return map[bool]string{true: "a failure without a system cause", false: "another error"}[errors.Unwrap(err) == nil] + ", class " + runtimeconfig.WindowsPrivateFailureClass(err)
}

// drillReserveWindowsCheck is one expectation: unless ok it fails with message and the class of err.
func drillReserveWindowsCheck(t *testing.T, ok bool, err error, message string) {
	t.Helper()
	if !ok {
		t.Fatal(message + "; got " + drillReserveWindowsClass(err))
	}
}

// drillReserveWindowsRoot is a new private directory on local uncompressed NTFS (R13), held until the row ends.
func drillReserveWindowsRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := testPrivateRoot(t)
	_, err := drillStorageOpened(t, dir)
	drillAssert(t, err == nil, "the reservation rows need an NTFS temporary directory")
	root, err := os.OpenRoot(dir)
	drillAssert(t, err == nil, "cannot hold the drill reserve fixture root")
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

// drillReserveWindowsFile is a private file created through the walk below root and allocated to size.
func drillReserveWindowsFile(t *testing.T, root *os.Root, name string, size uint64) *os.File {
	t.Helper()
	f, err := drillPlatformCreate(nil, nil, root, "", name)
	drillReserveWindowsCheck(t, err == nil, err, "the drill reserve fixture file was not created")
	t.Cleanup(func() { _ = f.Close() })
	for offset := uint64(0); offset < size; offset += 64 << 10 {
		err = drillPlatformAllocate(f, offset, min(64<<10, size-offset))
		drillReserveWindowsCheck(t, err == nil, err, "an end-of-file allocation failed")
	}
	return f
}

// drillReserveWindowsSizes reads f's FILE_STANDARD_INFO through its handle.
func drillReserveWindowsSizes(t *testing.T, f *os.File) drillReserveWindowsStandard {
	t.Helper()
	var standard drillReserveWindowsStandard
	err := windows.GetFileInformationByHandleEx(windows.Handle(f.Fd()), windows.FileStandardInfo, (*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard)))
	drillReserveWindowsCheck(t, err == nil, err, "cannot read the drill reserve fixture's standard information")
	return standard
}

// drillReserveWindowsControl reopens the closed fixture at name read-write by its path (ruling N1) and sends a control.
func drillReserveWindowsControl(t *testing.T, name string, code uint32, input []byte, message string) *os.File {
	t.Helper()
	f, err := runtimeconfig.WindowsPrivateOpenFile(name, os.O_RDWR)
	drillReserveWindowsCheck(t, err == nil, err, "cannot reopen the drill reserve fixture for its control")
	t.Cleanup(func() { _ = f.Close() })
	var returned uint32
	err = windows.DeviceIoControl(windows.Handle(f.Fd()), code, &input[0], uint32(len(input)), nil, 0, &returned, nil)
	drillReserveWindowsCheck(t, err == nil, err, message)
	return f
}

// drillReserveWindowsVolume: the volume root holding dir, its Volume{...}\ name and a target on another drive letter.
func drillReserveWindowsVolume(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	path, err := windows.UTF16PtrFromString(dir)
	volumeRoot, volumeName := make([]uint16, windows.MAX_PATH), make([]uint16, windows.MAX_PATH)
	drillAssert(t, err == nil && windows.GetVolumePathName(path, &volumeRoot[0], windows.MAX_PATH) == nil &&
		windows.GetVolumeNameForVolumeMountPoint(&volumeRoot[0], &volumeName[0], windows.MAX_PATH) == nil, "cannot read the drill reserve fixture's volume")
	rootPath, name := windows.UTF16ToString(volumeRoot), windows.UTF16ToString(volumeName)
	drillAssert(t, len(rootPath) >= 3 && strings.HasPrefix(name, `\\?\Volume{`), "the drill reserve fixture's volume has no volume name")
	drive := map[bool]string{false: "Z", true: "Y"}[strings.EqualFold(rootPath[:1], "Z")]
	return rootPath, strings.TrimPrefix(name, `\\?\`), drive + `:\drill-reserve-absent`
}

// drillReserveWindowsPresent reports, for each name, whether an object stands there, not followed.
func drillReserveWindowsPresent(names ...string) (present []bool) {
	for _, name := range names {
		_, err := os.Lstat(name)
		present = append(present, err == nil)
	}
	return present
}

// The Windows layer on the runner's NTFS (CI's localcell-windows job only); one check, one fixed text per expectation.
func TestDrillReservePlatformWindows(t *testing.T) {
	const drillID = "drill_aaaaaaaaaaaaaaaa"
	// The cell's recovery/ held, the drill directory made through it and held as a root by its anchor.
	cell := func(t *testing.T) (*checkpointVerifyCase, *os.Root, *os.Root) {
		v := newCheckpointVerifyCase(t, false)
		_, err := drillStorageOpened(t, v.c.recoveryDir())
		drillAssert(t, err == nil, "the reservation rows need an NTFS temporary directory")
		boundary, err := os.OpenRoot(v.c.recoveryDir() + drillRootHere)
		drillAssert(t, err == nil, "cannot hold the drill reserve fixture boundary")
		t.Cleanup(func() { _ = boundary.Close() })
		err = drillPlatformMkdir(v.c, boundary, nil, drillID, "")
		drillReserveWindowsCheck(t, err == nil, err, "the drill directory was not created")
		drill, err := drillReserveRoot(boundary, drillID)
		drillReserveWindowsCheck(t, err == nil, err, "the created drill directory was not held through its anchor")
		t.Cleanup(func() { _ = drill.Close() })
		return v, boundary, drill
	}
	t.Run("allocated", func(t *testing.T) {
		v, _, drill := cell(t)
		err := drillReserveWindowsFile(t, drill, "clone", 192<<10).Close()
		drillReserveWindowsCheck(t, err == nil, err, "the drill's file did not close")
		reopened, err := drillPlatformOpen(drill, "clone", false)
		drillReserveWindowsCheck(t, err == nil, err, "the drill's file was not reopened through the walk")
		t.Cleanup(func() { _ = reopened.Close() })
		err = drillPlatformVerify(reopened, 192<<10)
		drillReserveWindowsCheck(t, err == nil, err, "an allocated file was not proved on its reopened handle")
		drillReserveWindowsCheck(t, reopened.Close() == nil, nil, "the reopened file did not close")
		err = v.c.validateRecovery()
		drillReserveWindowsCheck(t, err == nil, err, "the created drill tree is not private")
	})
	// Ruling N1: the creators close a created directory's handle, so a walk through it and creations below it work.
	t.Run("nested", func(t *testing.T) {
		_, drill := drillReserveWindowsRoot(t)
		err := drillPlatformMkdir(nil, nil, drill, "", "sub")
		drillReserveWindowsCheck(t, err == nil, err, "a directory was not created")
		err = drillPlatformMkdir(nil, nil, drill, "", "sub/deeper")
		drillReserveWindowsCheck(t, err == nil, err, "a directory below a created directory was not created")
		err = drillReserveWindowsFile(t, drill, "sub/deeper/f", 4096).Close()
		drillReserveWindowsCheck(t, err == nil, err, "a file below created directories did not close")
		for _, directory := range []bool{false, true} {
			opened, err := drillPlatformOpen(drill, map[bool]string{false: "sub/deeper/f", true: "sub/deeper"}[directory], directory)
			drillReserveWindowsCheck(t, err == nil, err, map[bool]string{false: "a file below created directories was not reopened",
				true: "a created directory was not opened as a directory"}[directory])
			drillReserveWindowsCheck(t, opened.Close() == nil, nil, "an object opened below created directories did not close")
		}
	})
	t.Run("in_place", func(t *testing.T) {
		dir, drill := drillReserveWindowsRoot(t)
		err := drillReserveWindowsFile(t, drill, "clone", 192<<10).Close()
		drillReserveWindowsCheck(t, err == nil, err, "the drill reserve fixture did not close")
		reopened, err := drillPlatformOpen(drill, "clone", false)
		drillReserveWindowsCheck(t, err == nil, err, "the drill reserve fixture was not reopened through the walk")
		t.Cleanup(func() { _ = reopened.Close() })
		before := drillReserveWindowsSizes(t, reopened)
		writer, err := runtimeconfig.WindowsPrivateOpenFile(filepath.Join(dir, "clone"), os.O_WRONLY)
		drillReserveWindowsCheck(t, err == nil, err, "cannot reopen the drill reserve fixture without truncation")
		_, err = writer.Write(bytes.Repeat([]byte{0x5a}, 192<<10))
		drillReserveWindowsCheck(t, err == nil, err, "cannot write the drill reserve fixture's whole range")
		err = errors.Join(writer.Sync(), writer.Close())
		drillReserveWindowsCheck(t, err == nil, err, "cannot sync the drill reserve fixture's whole range")
		after := drillReserveWindowsSizes(t, reopened)
		drillReserveWindowsCheck(t, before.allocation >= 192<<10, nil, "the end-of-file allocation does not cover the range")
		drillReserveWindowsCheck(t, after.allocation == before.allocation, nil, "a full write changed the allocation")
		drillReserveWindowsCheck(t, after.end == 192<<10, nil, "a full write changed the end of file")
		err = drillPlatformVerify(reopened, 192<<10)
		drillReserveWindowsCheck(t, err == nil, err, "a written allocation was no longer proved")
	})
	t.Run("size_differs", func(t *testing.T) {
		_, drill := drillReserveWindowsRoot(t)
		f := drillReserveWindowsFile(t, drill, "clone", 8192)
		err := drillPlatformVerify(f, 8192)
		drillReserveWindowsCheck(t, err == nil, err, "an allocated file was not proved on its writer")
		err = drillPlatformVerify(f, 4096)
		drillReserveWindowsCheck(t, err == error(drillStorageError{reason: "allocation_unproved"}), err, "a file longer than its size was proved")
		err = drillPlatformVerify(f, 16384)
		drillReserveWindowsCheck(t, err == error(drillStorageError{reason: "allocation_unproved"}), err, "a file shorter than its size was proved")
	})
	t.Run("sparse_attribute", func(t *testing.T) {
		dir, drill := drillReserveWindowsRoot(t)
		err := drillReserveWindowsFile(t, drill, "clone", 0).Close()
		drillReserveWindowsCheck(t, err == nil, err, "the drill reserve fixture did not close")
		f := drillReserveWindowsControl(t, filepath.Join(dir, "clone"), windows.FSCTL_SET_SPARSE, []byte{1}, "cannot mark the drill reserve fixture sparse")
		err = drillPlatformAllocate(f, 0, 64<<10)
		drillReserveWindowsCheck(t, err == nil, err, "a sparse file's end of file was not set")
		err = drillPlatformVerify(f, 64<<10)
		drillReserveWindowsCheck(t, err == error(drillStorageError{reason: "allocation_unproved"}), err, "a sparse file was proved")
		_, err = f.Write(bytes.Repeat([]byte{0x5a}, 64<<10))
		drillReserveWindowsCheck(t, err == nil, err, "cannot write the sparse fixture's whole range")
		drillReserveWindowsCheck(t, f.Sync() == nil, nil, "cannot sync the sparse fixture")
		err = drillPlatformVerify(f, 64<<10)
		drillReserveWindowsCheck(t, err == error(drillStorageError{reason: "allocation_unproved"}), err, "a written sparse file was proved")
	})
	// FSCTL_SET_COMPRESSION with COMPRESSION_FORMAT_DEFAULT (1), as drill_storage_windows_test.go's row.
	t.Run("compressed_file", func(t *testing.T) {
		dir, drill := drillReserveWindowsRoot(t)
		err := drillReserveWindowsFile(t, drill, "clone", 0).Close()
		drillReserveWindowsCheck(t, err == nil, err, "the drill reserve fixture did not close")
		f := drillReserveWindowsControl(t, filepath.Join(dir, "clone"), windows.FSCTL_SET_COMPRESSION, []byte{1, 0},
			"cannot compress the drill reserve fixture (NTFS compression needs clusters of at most 4 KiB)")
		err = drillPlatformAllocate(f, 0, 64<<10)
		drillReserveWindowsCheck(t, err == nil, err, "a compressed file's end of file was not set")
		err = drillPlatformVerify(f, 64<<10)
		drillReserveWindowsCheck(t, err == error(drillStorageError{reason: "allocation_unproved"}), err, "a compressed file was proved")
	})
	t.Run("collision", func(t *testing.T) {
		v, boundary, drill := cell(t)
		err := drillReserveWindowsFile(t, drill, "clone", 0).Close()
		drillReserveWindowsCheck(t, err == nil, err, "the drill's file did not close")
		err = drillPlatformMkdir(v.c, boundary, drill, drillID, "sub")
		drillReserveWindowsCheck(t, err == nil, err, "the drill's directory was not created")
		again, err := drillPlatformCreate(v.c, boundary, drill, drillID, "clone")
		drillReserveWindowsNone(t, again, "a repeated file creation returned a file")
		drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "allocation_collision", err, "a repeated file creation was not a collision")
		err = drillPlatformMkdir(v.c, boundary, nil, drillID, "")
		drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "allocation_collision", err, "a repeated drill directory was not a collision")
		err = drillPlatformMkdir(v.c, boundary, drill, drillID, "sub")
		drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "allocation_collision", err, "a repeated directory was not a collision")
		below, err := drillPlatformCreate(v.c, boundary, drill, drillID, "absent/clone")
		drillReserveWindowsNone(t, below, "a create below an absent directory returned a file")
		drillReserveWindowsCheck(t, err != nil, err, "a create below an absent directory was not refused")
		drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "fallback", err, "a create below an absent directory had another reason")
		drillReserveWindowsCheck(t, errors.Is(err, fs.ErrNotExist), err, "an absent directory was not reported absent")
		err = runtimeconfig.WindowsPrivateValidate(filepath.Join(v.c.recoveryDir(), drillID, "clone"), false)
		drillReserveWindowsCheck(t, err == nil, err, "the created file's DACL is not the private one")
		err = v.c.validateRecovery()
		drillReserveWindowsCheck(t, err == nil, err, "the created drill tree is not private")
	})
	// An absent, file or junction component before the last is refused by each creator and the open; nothing changes.
	t.Run("link_component", func(t *testing.T) {
		volumeRoot, volume, drive := drillReserveWindowsVolume(t, t.TempDir())
		for _, kind := range []string{"absent", "file", "inside", "outside", "volume", "drive"} {
			t.Run(kind, func(t *testing.T) {
				dir, drill := drillReserveWindowsRoot(t)
				outside, component := testPrivateRoot(t), filepath.Join(dir, "a")
				// Each directory target holds the z the open asks for, so an open that followed would succeed.
				err := errors.Join(testMkdir(filepath.Join(dir, "b"), 0o700), testWriteFile(filepath.Join(dir, "b", "z"), []byte("z"), 0o600),
					testWriteFile(filepath.Join(outside, "z"), []byte("z"), 0o600))
				drillAssert(t, err == nil, "cannot prepare the junction fixture's targets")
				targets := map[string]string{"inside": filepath.Join(dir, "b"), "outside": outside, "volume": volume, "drive": drive}
				if kind == "file" {
					drillAssert(t, testWriteFile(component, []byte("a"), 0o600) == nil, "cannot plant the file component")
				} else if targets[kind] != "" {
					createTestJunction(t, component, targets[kind])
				}
				spots := []string{filepath.Join(volumeRoot, "x"), filepath.Join(volumeRoot, "y"), drive}
				before, beforeOutside, beforeSpots := windowsTreeSnapshot(t, dir), windowsTreeSnapshot(t, outside), drillReserveWindowsPresent(spots...)
				for _, call := range []struct {
					label string
					run   func() (*os.File, error)
				}{{"a directory creation", func() (*os.File, error) { return nil, drillPlatformMkdir(nil, nil, drill, "", "a/x") }},
					{"a file creation", func() (*os.File, error) { return drillPlatformCreate(nil, nil, drill, "", "a/y") }},
					{"an open", func() (*os.File, error) { return drillPlatformOpen(drill, "a/z", false) }}} {
					f, err := call.run()
					drillReserveWindowsNone(t, f, call.label+" through a planted component returned an object")
					drillReserveWindowsCheck(t, err != nil, err, call.label+" through a planted component was not refused")
					drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "fallback", err, call.label+" through a planted component had another reason")
					// R5: on Windows an absent component is a not-exist error, and it is still a refusal.
					drillReserveWindowsCheck(t, kind != "absent" || errors.Is(err, fs.ErrNotExist), err, call.label+" below an absent component was not reported absent")
				}
				drillAssert(t, reflect.DeepEqual(before, windowsTreeSnapshot(t, dir)), "a planted component's root changed")
				drillAssert(t, reflect.DeepEqual(beforeOutside, windowsTreeSnapshot(t, outside)), "a junction's outside target changed")
				drillAssert(t, reflect.DeepEqual(beforeSpots, drillReserveWindowsPresent(spots...)), "a junction's volume or drive target changed")
			})
		}
	})
	// The anchor holds a private directory; a junction, an inherited (not private) directory and an absent name are refused.
	t.Run("anchor", func(t *testing.T) {
		dir, parent := drillReserveWindowsRoot(t)
		target := testPrivateRoot(t)
		drillAssert(t, testMkdir(filepath.Join(dir, "plain"), 0o700) == nil && os.Mkdir(filepath.Join(dir, "inherited"), 0o700) == nil,
			"cannot prepare the drill reserve anchor fixture")
		createTestJunction(t, filepath.Join(dir, "d"), target)
		before, beforeTarget := windowsTreeSnapshot(t, dir), windowsTreeSnapshot(t, target)
		root, err := drillReserveRoot(parent, "plain")
		drillReserveWindowsCheck(t, err == nil, err, "a private directory was not held as a root")
		info, statErr := root.Stat(".")
		named, namedErr := os.Lstat(filepath.Join(dir, "plain"))
		drillAssert(t, errors.Join(statErr, namedErr, root.Close()) == nil, "cannot read the held anchor root's identity")
		drillAssert(t, os.SameFile(info, named), "the held root is not the directory at its name")
		for _, refused := range []struct{ name, message string }{{"d", "a junction at the drill's name was held"},
			{"inherited", "a directory that is not private was held"}, {"absent", "an absent drill name was held"}} {
			root, err := drillReserveRoot(parent, refused.name)
			if root != nil {
				_ = root.Close()
			}
			drillReserveWindowsCheck(t, root == nil, nil, refused.message+" as a root")
			drillReserveWindowsCheck(t, err != nil, err, refused.message)
			drillReserveWindowsCheck(t, drillReserveReason(err, "fallback") == "fallback", err, refused.message+" with another reason")
		}
		drillAssert(t, reflect.DeepEqual(before, windowsTreeSnapshot(t, dir)), "the anchor changed its parent")
		drillAssert(t, reflect.DeepEqual(beforeTarget, windowsTreeSnapshot(t, target)), "the anchor changed a junction's target")
	})
	t.Run("malformed", func(t *testing.T) {
		dir, root := drillReserveWindowsRoot(t)
		drillAssert(t, testMkdir(filepath.Join(dir, "b"), 0o700) == nil, "cannot prepare the malformed fixture")
		before := windowsTreeSnapshot(t, dir)
		err := drillPlatformMkdir(nil, root, nil, "b/x", "")
		drillReserveWindowsCheck(t, err == error(windows.ERROR_INVALID_NAME), err, "an identifier of two components was not refused as a bare invalid name")
		for _, malformed := range []struct{ id, name, label string }{{`b\x`, "", "a backslash identifier"}, {"x:y", "", "a colon identifier"},
			{"..", "", "a parent identifier"}, {"", `a\b`, "a backslash name"}, {"", "a:b", "a colon name"}} {
			calls := map[string]func() (*os.File, error){"mkdir": func() (*os.File, error) { return nil, drillPlatformMkdir(nil, root, nil, malformed.id, "") }}
			if malformed.name != "" {
				calls = map[string]func() (*os.File, error){"mkdir": func() (*os.File, error) { return nil, drillPlatformMkdir(nil, nil, root, "", malformed.name) },
					"create":         func() (*os.File, error) { return drillPlatformCreate(nil, nil, root, "", malformed.name) },
					"open":           func() (*os.File, error) { return drillPlatformOpen(root, malformed.name, false) },
					"open directory": func() (*os.File, error) { return drillPlatformOpen(root, malformed.name, true) }}
			}
			for call, run := range calls {
				f, err := run()
				drillReserveWindowsNone(t, f, malformed.label+" returned an object from "+call)
				drillReserveWindowsCheck(t, err != nil, err, malformed.label+" was not refused by "+call)
				drillReserveWindowsCheck(t, errors.Unwrap(err) == nil, err, malformed.label+" reached a system call in "+call)
			}
		}
		drillAssert(t, reflect.DeepEqual(before, windowsTreeSnapshot(t, dir)), "a malformed name created something")
	})
	t.Run("refusal", func(t *testing.T) {
		for errno, message := range map[syscall.Errno]string{windows.ERROR_DISK_FULL: "a full disk was not storage_exhausted",
			windows.ERROR_HANDLE_DISK_FULL: "a full handle disk was not storage_exhausted", windows.ERROR_DISK_QUOTA_EXCEEDED: "an exceeded quota was not storage_exhausted",
			windows.ERROR_FILE_TOO_LARGE: "a file too large was not storage_exhausted", windows.ERROR_ACCESS_DENIED: "access denied was named a platform refusal"} {
			want := map[bool]string{false: "storage_exhausted"}[errno == windows.ERROR_ACCESS_DENIED]
			drillAssert(t, drillPlatformRefusal(errno) == want, message)
			drillAssert(t, drillPlatformRefusal(&os.SyscallError{Syscall: "SetFileInformationByHandle", Err: errno}) == want, message+" when wrapped")
		}
	})
	t.Run("constants", func(t *testing.T) {
		drillAssert(t, drillWindowsRefused == windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_SPARSE_FILE|windows.FILE_ATTRIBUTE_REPARSE_POINT|
			windows.FILE_ATTRIBUTE_COMPRESSED|windows.FILE_ATTRIBUTE_ENCRYPTED|windows.FILE_ATTRIBUTE_INTEGRITY_STREAM, "the refused attributes differ from the system's")
		var standard drillReserveWindowsStandard
		drillAssert(t, unsafe.Sizeof(standard) == 24, "the standard information is not FILE_STANDARD_INFO's 24 bytes")
		drillAssert(t, unsafe.Offsetof(standard.end) == 8 && unsafe.Offsetof(standard.links) == 16, "the standard information sizes are not where FILE_STANDARD_INFO has them")
		drillAssert(t, unsafe.Offsetof(standard.deletePending) == 20 && unsafe.Offsetof(standard.directory) == 21, "the standard information flags are not where FILE_STANDARD_INFO has them")
	})
}

// drillReserveWindowsNone requires that a refused call returned no object, closing one it did return.
func drillReserveWindowsNone(t *testing.T, f *os.File, message string) {
	t.Helper()
	if f != nil {
		_ = f.Close()
		t.Fatal(message)
	}
}
