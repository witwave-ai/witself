//go:build windows

package runtimeconfig

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateSnapshotContainment(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		path := filepath.Join(windowsPrivateTestRoot(t), "record")
		original, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY)
		if err != nil {
			t.Fatal("cannot prepare snapshot identity control")
		}
		var want windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(windows.Handle(original.Fd()), &want)
		if closeErr := original.Close(); err != nil || closeErr != nil {
			t.Fatal("cannot identify snapshot identity control")
		}

		file, err := WindowsPrivateOpenSnapshot(path)
		if file == nil && err != nil {
			t.Logf("refused")
			t.Fatal("private snapshot refused its ordinary object")
		}
		if file == nil {
			t.Fatal("snapshot returned no result")
		}
		defer func() { _ = file.Close() }()
		var got windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &got) != nil {
			t.Fatal("cannot identify returned snapshot")
		}
		if got.VolumeSerialNumber != want.VolumeSerialNumber || got.FileIndexHigh != want.FileIndexHigh || got.FileIndexLow != want.FileIndexLow {
			t.Logf("escaped")
			t.Fatal("private snapshot left its pinned chain")
		}
		if err != nil {
			t.Fatal("snapshot returned a handle on refusal")
		}
		t.Logf("contained")
	})
	// While the chain is pinned, one held directory is emptied and converted
	// in place to a junction whose target holds a same-named decoy. The open
	// may be refused or may return the original object; it must never return
	// another one, with the directory put back before the final check or not.
	t.Run("converted_parent", func(t *testing.T) { snapshotContainmentProbe(t, false, true) })
	t.Run("converted_ancestor", func(t *testing.T) { snapshotContainmentProbe(t, true, true) })
	t.Run("converted_parent_kept", func(t *testing.T) { snapshotContainmentProbe(t, false, false) })
	t.Run("converted_ancestor_kept", func(t *testing.T) { snapshotContainmentProbe(t, true, false) })
	// The open leaf shares delete, so its directory can be emptied and turned
	// into a junction after the leaf open and before validation. The final
	// check of the held chain must refuse; nothing else in the suite reaches it.
	t.Run("converted_after_open", func(t *testing.T) {
		root := windowsPrivateTestRoot(t)
		inner, target, aside := filepath.Join(root, "inner"), filepath.Join(root, "target"), filepath.Join(root, "aside")
		if WindowsPrivateMkdir(inner) != nil || WindowsPrivateMkdir(target) != nil {
			t.Fatal("cannot prepare containment fixture layout")
		}
		path := filepath.Join(inner, "record")
		created, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY)
		if err != nil || created.Close() != nil {
			t.Fatal("cannot prepare containment fixture object")
		}
		buffer := spoolJunctionData(t, target)
		converted := false
		t.Cleanup(func() {
			if !converted {
				return
			}
			if writer, err := snapshotAttributeWriter(inner); err == nil {
				_ = clearSpoolJunction(writer)
				_ = windows.CloseHandle(writer)
			}
		})
		file, err := windowsPrivateSnapshot(path, true, nil, func() {
			if os.Rename(path, aside) != nil {
				return
			}
			writer, err := snapshotAttributeWriter(inner)
			if err != nil {
				return
			}
			converted = setSpoolJunction(writer, buffer) == nil
			_ = windows.CloseHandle(writer)
		})
		if file != nil {
			_ = file.Close()
		}
		if !converted {
			t.Fatal("cannot convert the held directory in place")
		}
		if file != nil || err == nil {
			t.Logf("snapshot containment class=%s", "admitted_converted")
			t.Fatal("private snapshot admitted a converted chain")
		}
		t.Logf("snapshot containment class=%s", "refused")
		snapshotRefusalCause(t, err)
	})
	// What the file system answers when a directory that still has an entry is
	// asked to become a junction. Nothing about admission is asserted here.
	t.Run("nonempty_conversion", func(t *testing.T) {
		root := windowsPrivateTestRoot(t)
		dir, target := filepath.Join(root, "full"), filepath.Join(root, "target")
		if WindowsPrivateMkdir(dir) != nil || WindowsPrivateMkdir(target) != nil {
			t.Fatal("cannot prepare conversion fixture layout")
		}
		file, err := WindowsPrivateOpenFile(filepath.Join(dir, "record"), os.O_CREATE|os.O_EXCL|os.O_RDONLY)
		if err != nil || file.Close() != nil {
			t.Fatal("cannot prepare conversion fixture entry")
		}
		writer, err := snapshotAttributeWriter(dir)
		if err != nil {
			t.Fatal("cannot open the conversion fixture for attributes")
		}
		err = setSpoolJunction(writer, spoolJunctionData(t, target))
		class := "refused_other"
		switch {
		case err == nil:
			class = "allowed"
			_ = clearSpoolJunction(writer)
		case errors.Is(err, windows.ERROR_DIR_NOT_EMPTY):
			class = "refused_not_empty"
		}
		_ = windows.CloseHandle(writer)
		t.Logf("snapshot conversion class=%s", class)
	})
}

// The attribute-only writer of TestWindowsPrivateBeneath/convert_held: no
// sharing mode of a held directory handle can deny it.
func snapshotAttributeWriter(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(pointer, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// Two fixed classes say why a converted chain was refused: the private
// failure class, and whether the name was simply absent from the emptied
// directory's own index.
func snapshotRefusalCause(t *testing.T, err error) {
	t.Helper()
	absent := "present"
	if errors.Is(err, os.ErrNotExist) {
		absent = "absent"
	}
	t.Logf("snapshot refusal class=%s", WindowsPrivateFailureClass(err))
	t.Logf("snapshot refusal name class=%s", absent)
}

// What the file system itself answers to the reader's kind of open, one name
// relative to a directory handle taken before that directory was emptied and
// converted, without the reader's error mapping. In the parent case the decoy
// is the object a followed junction would reach.
func snapshotRawOpenClass(parent windows.Handle, name string, decoy windows.ByHandleFileInformation) string {
	object, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return "other"
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	handle := windows.InvalidHandle
	err = windows.NtCreateFile(&handle, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err == nil {
		var got windows.ByHandleFileInformation
		infoErr := windows.GetFileInformationByHandle(handle, &got)
		_ = windows.CloseHandle(handle)
		if infoErr == nil && got.VolumeSerialNumber == decoy.VolumeSerialNumber && got.FileIndexHigh == decoy.FileIndexHigh && got.FileIndexLow == decoy.FileIndexLow {
			return "opened_decoy"
		}
		return "opened_other"
	}
	var status windows.NTStatus
	if !errors.As(err, &status) {
		return "other"
	}
	switch status {
	case windows.STATUS_OBJECT_NAME_NOT_FOUND:
		return "name_not_found"
	case windows.STATUS_OBJECT_PATH_NOT_FOUND:
		return "path_not_found"
	case windows.STATUS_REPARSE, windows.STATUS_STOPPED_ON_SYMLINK, windows.STATUS_REPARSE_POINT_ENCOUNTERED,
		windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED, windows.STATUS_IO_REPARSE_DATA_INVALID, windows.STATUS_REPARSE_POINT_NOT_RESOLVED:
		return "reparse_status"
	}
	return "other"
}

func clearSpoolJunction(handle windows.Handle) error {
	var buffer [8]byte
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_DELETE_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
}

func snapshotContainmentProbe(t *testing.T, ancestor, restore bool) {
	root := windowsPrivateTestRoot(t)
	outer, target, aside := filepath.Join(root, "outer"), filepath.Join(root, "target"), filepath.Join(root, "aside")
	inner := filepath.Join(outer, "inner")
	for _, dir := range []string{outer, inner, target} {
		if WindowsPrivateMkdir(dir) != nil {
			t.Fatal("cannot prepare containment fixture layout")
		}
	}
	identify := func(path string) windows.ByHandleFileInformation {
		var info windows.ByHandleFileInformation
		file, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY)
		if err == nil {
			err = windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info)
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			t.Fatal("cannot prepare containment fixture object")
		}
		return info
	}
	path := filepath.Join(inner, "record")
	want := identify(path)
	// converted is the held directory changed in place; moved is its only
	// entry, which waits at aside while the directory is a junction.
	converted, moved, decoyPath, offset := inner, path, filepath.Join(target, "record"), 1
	if ancestor {
		converted, moved, decoyPath, offset = outer, inner, filepath.Join(target, "inner", "record"), 2
		if WindowsPrivateMkdir(filepath.Join(target, "inner")) != nil {
			t.Fatal("cannot prepare containment decoy layout")
		}
	}
	decoy := identify(decoyPath)
	buffer := spoolJunctionData(t, target)
	raw := "not_run"
	convert := func() bool {
		// A handle taken before the conversion, as the reader's own is.
		pointer, err := windows.UTF16PtrFromString(converted)
		if err != nil {
			return false
		}
		before, err := windows.CreateFile(pointer, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return false
		}
		defer func() { _ = windows.CloseHandle(before) }()
		if os.Rename(moved, aside) != nil {
			return false
		}
		writer, err := snapshotAttributeWriter(converted)
		if err != nil {
			return false
		}
		err = setSpoolJunction(writer, buffer)
		if windows.CloseHandle(writer) != nil || err != nil {
			return false
		}
		raw = snapshotRawOpenClass(before, filepath.Base(moved), decoy)
		return true
	}
	revert := func() bool {
		writer, err := snapshotAttributeWriter(converted)
		if err != nil {
			return false
		}
		err = clearSpoolJunction(writer)
		return windows.CloseHandle(writer) == nil && err == nil && os.Rename(aside, moved) == nil
	}
	attempted, didConvert, didRevert := false, false, false
	t.Cleanup(func() {
		if didConvert && !didRevert {
			_ = revert()
		}
	})
	file, err := windowsPrivateSnapshot(path, true, func(index, total int) {
		if index == total-offset && !attempted {
			attempted = true
			didConvert = convert()
		}
	}, func() {
		if restore && didConvert {
			didRevert = revert()
		}
	})
	if file != nil {
		defer func() { _ = file.Close() }()
	}
	if !didConvert {
		t.Fatal("cannot convert the held directory in place")
	}
	t.Logf("snapshot raw open class=%s", raw)
	if file == nil {
		if err == nil {
			t.Fatal("snapshot returned no result")
		}
		t.Logf("snapshot containment class=%s", "refused")
		snapshotRefusalCause(t, err)
		return
	}
	var got windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &got) != nil {
		t.Fatal("cannot identify returned snapshot")
	}
	same := func(a, b windows.ByHandleFileInformation) bool {
		return a.VolumeSerialNumber == b.VolumeSerialNumber && a.FileIndexHigh == b.FileIndexHigh && a.FileIndexLow == b.FileIndexLow
	}
	if same(got, want) && restore && didRevert {
		t.Logf("snapshot containment class=%s", "contained")
		return
	}
	if same(got, want) {
		t.Logf("snapshot containment class=%s", "admitted_converted")
		t.Fatal("private snapshot admitted a converted chain")
	}
	if same(got, decoy) {
		t.Logf("snapshot containment class=%s", "escaped")
	} else {
		t.Logf("snapshot containment class=%s", "escaped_other")
	}
	t.Fatal("private snapshot left its pinned chain")
}

func TestWindowsPrivateSnapshot(t *testing.T) {
	fixture := func(t *testing.T) (root, path string) {
		t.Helper()
		root = windowsPrivateTestRoot(t)
		path = filepath.Join(root, "record")
		file, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR)
		if err != nil {
			t.Fatal("cannot prepare snapshot fixture")
		}
		_, err = file.Write([]byte{1, 2, 3})
		if closeErr := file.Close(); err != nil || closeErr != nil {
			t.Fatal("cannot finish snapshot fixture")
		}
		return root, path
	}

	t.Run("retained_validation", func(t *testing.T) {
		root, path := fixture(t)
		renamed := false
		file, err := windowsPrivateSnapshot(path, true, nil, func() {
			renamed = WindowsPrivateRename(path, filepath.Join(root, "retired")) == nil
		})
		if file != nil {
			defer func() { _ = file.Close() }()
		}
		if !renamed || err != nil || file == nil {
			t.Fatal("private snapshot lost its complete object")
		}
		data, err := io.ReadAll(file)
		if err != nil || string(data) != string([]byte{1, 2, 3}) {
			t.Fatal("private snapshot lost its complete object")
		}
	})
	t.Run("read_only", func(t *testing.T) {
		_, path := fixture(t)
		file, err := WindowsPrivateOpenSnapshot(path)
		if err != nil {
			t.Fatal("cannot open snapshot control")
		}
		defer func() { _ = file.Close() }()
		if _, err := file.Write([]byte{4}); err == nil {
			t.Fatal("private snapshot permitted a write")
		}
	})
	t.Run("leaf_dacl", func(t *testing.T) {
		_, path := fixture(t)
		windowsPrivateTestBroaden(t, path)
		file, err := WindowsPrivateOpenSnapshot(path)
		if file != nil {
			_ = file.Close()
		}
		if file != nil || !errors.Is(err, errWindowsPrivateACECount) {
			t.Fatal("private snapshot admitted an unproved object")
		}
	})
	t.Run("parent_dacl", func(t *testing.T) {
		root, path := fixture(t)
		windowsPrivateTestBroaden(t, root)
		file, err := WindowsPrivateOpenSnapshot(path)
		if file != nil {
			_ = file.Close()
		}
		if file != nil || !errors.Is(err, errWindowsPrivateACECount) {
			t.Fatal("private snapshot admitted an unproved object")
		}
	})
	t.Run("tree_leaf_policy", func(t *testing.T) {
		root, path := fixture(t)
		windowsPrivateTestBroaden(t, root)
		if WindowsPrivateValidateSnapshot(path) != nil {
			t.Fatal("tree entry changed its privacy boundary")
		}
	})
	t.Run("retained_parent", func(t *testing.T) {
		root, path := fixture(t)
		retired := root + "-retired"
		var renameErr error
		file, err := windowsPrivateSnapshot(path, true, nil, func() { renameErr = os.Rename(root, retired) })
		if file != nil {
			_ = file.Close()
		}
		blocked := errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) || errors.Is(renameErr, windows.ERROR_ACCESS_DENIED)
		if !blocked || err != nil || file == nil || os.Rename(root, retired) != nil {
			t.Fatal("private snapshot did not pin its parent through the open")
		}
	})
	// Before the leaf is opened nothing below the parent is open, so only the
	// reader's own handle on the parent can refuse its rename.
	t.Run("pinned_before_open", func(t *testing.T) {
		root, path := fixture(t)
		retired := root + "-retired"
		attempted := false
		var renameErr error
		file, err := windowsPrivateSnapshot(path, true, func(index, total int) {
			if index != total-1 || attempted {
				return
			}
			attempted = true
			if renameErr = os.Rename(root, retired); renameErr == nil {
				_ = os.Rename(retired, root)
			}
		}, nil)
		if file != nil {
			_ = file.Close()
		}
		blocked := errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) || errors.Is(renameErr, windows.ERROR_ACCESS_DENIED)
		if !attempted || !blocked || err != nil || file == nil {
			t.Fatal("private snapshot did not pin its parent before the open")
		}
	})
	t.Run("reparse_attribute", func(t *testing.T) {
		root := windowsPrivateTestRoot(t)
		path := filepath.Join(root, "junction")
		if WindowsPrivateMkdir(path) != nil {
			t.Fatal("cannot prepare reparse attribute control")
		}
		makeSpoolJunction(t, path, t.TempDir())
		handle, err := windowsPrivateOpenMetadata(path, windows.READ_CONTROL)
		if err != nil {
			t.Fatal("cannot open reparse attribute control")
		}
		defer func() { _ = windows.CloseHandle(handle) }()
		// Match the directory type so that the reparse attribute alone must
		// refuse this otherwise private handle in the snapshot's validator.
		if windowsPrivateValidateHandle(handle, true) == nil {
			t.Fatal("private snapshot admitted an unproved object")
		}
	})
}
