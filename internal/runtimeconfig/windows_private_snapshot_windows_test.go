//go:build windows

package runtimeconfig

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

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
			return
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
		file, err := windowsPrivateSnapshot(path, true, func() {
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
		file, err := windowsPrivateSnapshot(path, true, func() { renameErr = os.Rename(root, retired) })
		if file != nil {
			_ = file.Close()
		}
		blocked := errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) || errors.Is(renameErr, windows.ERROR_ACCESS_DENIED)
		if !blocked || err != nil || file == nil || os.Rename(root, retired) != nil {
			t.Fatal("private snapshot did not pin its parent through the open")
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
