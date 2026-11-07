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

func TestWindowsPrivateRenameWithOpenReaders(t *testing.T) {
	for _, shareDelete := range []bool{true, false} {
		name := "sharing-denied-preserves-both-files"
		if shareDelete {
			name = "reader-retains-replaced-object"
		}
		t.Run(name, func(t *testing.T) {
			root := windowsPrivateTestRoot(t)
			target := filepath.Join(root, "target")
			const before, after = "synthetic previous state", "synthetic replacement state"
			initial, err := WindowsPrivateOpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := initial.WriteString(before); err != nil {
				t.Fatal("cannot write original publication fixture")
			}
			if err := initial.Close(); err != nil {
				t.Fatal("cannot close original publication fixture")
			}
			source, err := WindowsPrivateCreateTemp(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			if _, err := source.WriteString(after); err != nil || source.Sync() != nil {
				t.Fatal("cannot persist replacement publication fixture")
			}
			pointer, err := windows.UTF16PtrFromString(target)
			if err != nil {
				t.Fatal("cannot encode publication fixture")
			}
			share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
			if shareDelete {
				share |= windows.FILE_SHARE_DELETE
			}
			handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, share, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				t.Fatal("cannot open publication reader")
			}
			reader := os.NewFile(uintptr(handle), target)
			defer func() { _ = reader.Close() }()
			// Leave the production-created source writer open too: its reopen
			// must grant delete sharing while the old target reader is retained.
			err = WindowsPrivateRename(source.Name(), target)
			want := before
			if shareDelete {
				if err != nil {
					t.Error("private replacement failed with delete sharing permitted")
					t.Fatal(err)
				}
				want = after
				if _, err := os.Lstat(source.Name()); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("private replacement retained its old source name")
				}
			} else {
				if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					t.Fatal("private replacement ignored delete sharing denial")
				}
				data, err := os.ReadFile(source.Name())
				if err != nil || string(data) != after {
					t.Fatal("sharing denial changed the unpublished source")
				}
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != want {
				t.Fatal("private replacement lost the expected target state")
			}
			data, err = io.ReadAll(reader)
			if err != nil || string(data) != before {
				t.Fatal("private replacement changed the retained reader state")
			}
			assertSpoolPrivateDescriptor(t, target)
		})
	}
}

func TestWindowsPrivateRollbackDoesNotFollowReparseLeaf(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	target := windowsPrivateTestRoot(t)
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal("cannot inspect rollback target fixture")
	}
	witness := filepath.Join(target, "witness")
	const content = "synthetic rollback target witness"
	if err := os.WriteFile(witness, []byte(content), 0o600); err != nil {
		t.Fatal("cannot write rollback target witness")
	}
	junction := filepath.Join(root, "junction")
	if err := WindowsPrivateMkdir(junction); err != nil {
		t.Fatal("cannot create rollback junction fixture")
	}
	makeSpoolJunction(t, junction, target)
	handle, err := windowsPrivateOpenMetadata(junction, windows.DELETE)
	if err != nil {
		t.Fatal("cannot open rollback reparse leaf")
	}
	defer func() {
		if handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatal("rollback metadata handle followed the reparse target")
	}
	if err := windowsPrivateRollback(handle, errWindowsPrivateReparse); !errors.Is(err, errWindowsPrivateReparse) {
		t.Fatal("rollback could not remove the reparse leaf")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal("cannot close rollback reparse handle")
	}
	handle = windows.InvalidHandle
	if _, err := os.Lstat(junction); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback retained the rejected junction")
	}
	afterInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(before, afterInfo) {
		t.Fatal("rollback changed the reparse target identity")
	}
	data, err := os.ReadFile(witness)
	if err != nil || string(data) != content {
		t.Fatal("rollback changed the reparse target contents")
	}
	assertSpoolPrivateDescriptor(t, target)
}

// The source DELETE handle remains open through post-rename validation in the
// production publisher. Model that exact primitive window without weakening
// the private reader's sharing or path protections.
func TestWindowsPrivateRenameVisibleBeforePrivateReadable(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	target := filepath.Join(root, "published")
	const content = "synthetic complete publication"
	source, err := WindowsPrivateCreateTemp(root)
	if err != nil {
		t.Fatal("cannot create staged publication fixture")
	}
	defer func() { _ = source.Close() }()
	if _, err := source.WriteString(content); err != nil || source.Sync() != nil {
		t.Fatal("cannot persist staged publication fixture")
	}
	if source.Close() != nil {
		t.Fatal("cannot close staged publication fixture")
	}
	handle, err := windowsPrivateOpenMetadata(source.Name(), windows.DELETE|windows.READ_CONTROL)
	if err != nil {
		t.Fatal("cannot retain publication metadata handle")
	}
	defer func() {
		if handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}()
	if windowsPrivateValidateHandle(handle, false) != nil || windowsPrivateReplace(handle, target) != nil {
		t.Fatal("cannot publish retained-handle fixture")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("published name was not visible with retained DELETE handle")
	}
	reader, err := WindowsPrivateOpenFile(target, os.O_RDONLY)
	if reader != nil {
		_ = reader.Close()
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("visible publication did not refuse private open while DELETE handle was retained")
	}
	if windows.CloseHandle(handle) != nil {
		t.Fatal("cannot release publication metadata handle")
	}
	handle = windows.InvalidHandle
	reader, err = WindowsPrivateOpenFile(target, os.O_RDONLY)
	if err != nil {
		t.Fatal("settled publication remained privately unreadable")
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != content {
		t.Fatal("settled publication did not retain complete bytes")
	}
	assertSpoolPrivateDescriptor(t, target)
}
