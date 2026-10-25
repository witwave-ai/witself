//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateGuardRetainsDirectoryIdentity(t *testing.T) {
	for _, absentLeaf := range []bool{false, true} {
		name := "existing_directory"
		if absentLeaf {
			name = "parent_of_absent_leaf"
		}
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "ancestor")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(directory)
			if err != nil {
				t.Fatal(err)
			}
			path := directory
			if absentLeaf {
				path = filepath.Join(directory, "absent")
			}
			guard, _, err := windowsPrivateGuard(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { guard.close() }()
			retired := directory + "-retired"
			if err := os.Rename(directory, retired); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("retained directory guard did not refuse rename")
			}
			after, err := os.Stat(directory)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("guarded directory identity changed")
			}
			guard.close()
			guard = nil
			if err := os.Rename(directory, retired); err != nil {
				t.Fatal("directory rename remained blocked after guard close")
			}
		})
	}
}

// Attribute-only writers can change an empty guarded ancestor in place. Test
// the real creation interval and require detection plus handle-bound rollback,
// rather than assuming the sharing fence prevents the reparse operation.
func TestWindowsPrivateGuardRefusesInPlaceReparse(t *testing.T) {
	for _, test := range []struct {
		name    string
		access  uint32
		preopen bool
	}{
		{"data_writer", windows.GENERIC_WRITE, false},
		{"attributes_writer", windows.FILE_WRITE_ATTRIBUTES, false},
		{"preopened_attributes_writer", windows.FILE_WRITE_ATTRIBUTES, true},
	} {
		for _, directory := range []bool{true, false} {
			kind := "file"
			if directory {
				kind = "directory"
			}
			t.Run(test.name+"/"+kind, func(t *testing.T) {
				parent := t.TempDir()
				ancestor, target := filepath.Join(parent, "ancestor"), filepath.Join(parent, "target")
				for _, path := range []string{ancestor, target} {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal("cannot create reparse fixture")
					}
				}
				buffer := spoolJunctionData(t, target)
				pointer, err := windows.UTF16PtrFromString(ancestor)
				if err != nil {
					t.Fatal("cannot encode reparse fixture")
				}
				openWriter := func(access uint32) (windows.Handle, error) {
					return windows.CreateFile(pointer, access,
						windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
						nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				}
				writer := windows.InvalidHandle
				defer func() {
					if writer != windows.InvalidHandle {
						_ = windows.CloseHandle(writer)
					}
				}()
				if test.preopen {
					writer, err = openWriter(test.access)
					if err != nil {
						t.Fatal("cannot open attribute-only control handle")
					}
				}
				attempted, redirected := false, false
				beforeCreate := func() error {
					attempted = true
					if !test.preopen {
						writer, err = openWriter(test.access)
					}
					if err == nil {
						err = setSpoolJunction(writer, buffer)
					}
					if err != nil && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
						t.Fatal("reparse attempt failed for an unrelated fixture reason")
					}
					redirected = err == nil
					return nil
				}
				path := filepath.Join(ancestor, "created")
				var createErr error
				if directory {
					createErr = windowsPrivateMkdirWithHook(path, nil, beforeCreate)
				} else {
					var file *os.File
					file, createErr = windowsPrivateOpenFileWithHook(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, nil, beforeCreate)
					if file != nil {
						_ = file.Close()
						if redirected {
							t.Fatal("redirected creation exposed a writable handle")
						}
					}
				}
				if !attempted {
					t.Fatal("creation did not reach the guarded race seam")
				}
				if redirected {
					if !errors.Is(createErr, errWindowsPrivateReparse) {
						t.Fatal("redirected creation was not refused by the reparse check")
					}
					entries, readErr := os.ReadDir(target)
					if readErr != nil || len(entries) != 0 {
						t.Fatal("redirected creation was not rolled back")
					}
					return
				}
				if createErr != nil {
					t.Fatal(createErr)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal("cannot clear the unredirected control object")
				}
				if writer != windows.InvalidHandle {
					_ = windows.CloseHandle(writer)
				}
				writer, err = openWriter(windows.GENERIC_WRITE)
				if err != nil || setSpoolJunction(writer, buffer) != nil {
					t.Fatal("unguarded reparse positive control failed")
				}
			})
		}
	}
}

func TestWindowsPrivateGuardChecksEveryIdentityField(t *testing.T) {
	for _, field := range []string{"volume", "index_high", "index_low", "reopened_parent"} {
		t.Run(field, func(t *testing.T) {
			root := windowsPrivateTestRoot(t)
			file, err := WindowsPrivateOpenFile(filepath.Join(root, "file"), os.O_CREATE|os.O_EXCL|os.O_RDWR)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			guard, _, err := windowsPrivateGuard(root)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.close()
			if err := guard.verify(file.Name(), windows.Handle(file.Fd()), false); err != nil {
				t.Fatal(err)
			}
			last := &guard[len(guard)-1]
			switch field {
			case "volume":
				last.identity.VolumeSerialNumber++
			case "index_high":
				last.identity.FileIndexHigh++
			case "index_low":
				last.identity.FileIndexLow++
			case "reopened_parent":
				last.path = t.TempDir()
			}
			if !errors.Is(guard.verify(file.Name(), windows.Handle(file.Fd()), false), errWindowsPrivatePathChanged) {
				t.Fatal("changed parent identity escaped verification")
			}
		})
	}
}

func TestWindowsPrivateCreationRollsBackReparseLeaf(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	target := t.TempDir()
	path := filepath.Join(root, "created")
	err := windowsPrivateMkdir(path, func(created string) error {
		pointer, err := windows.UTF16PtrFromString(created)
		if err != nil {
			return err
		}
		writer, err := windows.CreateFile(pointer, windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return err
		}
		defer func() { _ = windows.CloseHandle(writer) }()
		return setSpoolJunction(writer, spoolJunctionData(t, target))
	})
	if !errors.Is(err, errWindowsPrivateReparse) {
		t.Fatal("created reparse leaf escaped verification")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created reparse leaf was not rolled back")
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatal("rollback removed the junction target")
	}
}

func TestWindowsPrivateRollbackKeepsReplacement(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	path, moved := filepath.Join(root, "created"), filepath.Join(root, "moved")
	file, err := windowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, func(windows.Handle) error {
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("replacement"), 0o600)
	})
	if file != nil {
		_ = file.Close()
		t.Fatal("replacement escaped creation verification")
	}
	if !errors.Is(err, errWindowsPrivatePathChanged) {
		t.Fatal("replacement was not refused by identity")
	}
	if _, err := os.Stat(moved); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created object was not rolled back by handle")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatal("rollback changed the replacement object")
	}
}

func TestWindowsPrivateReturnedWriterAllowsGuard(t *testing.T) {
	path := filepath.Join(windowsPrivateTestRoot(t), "created")
	file, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	// The rollback open requests DELETE. Its file object must be closed before
	// returning a writer, otherwise a later guard denying delete sharing fails.
	if err := WindowsPrivateValidate(path, false); err != nil {
		t.Fatal("returned writer retained rollback sharing access")
	}
	other, err := WindowsPrivateOpenFile(path, os.O_RDWR)
	if err != nil {
		t.Fatal("returned writer blocked a verified existing file open")
	}
	if err := other.Close(); err != nil {
		t.Fatal("cannot close verified existing file open")
	}
}
