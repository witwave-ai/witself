//go:build windows

package runtimeconfig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// beneathCalls are the walk's four forms: create a directory, create a file,
// open a file, open a directory.
var beneathCalls = []struct{ create, directory bool }{{true, true}, {true, false}, {false, false}, {false, true}}

func TestWindowsPrivateBeneath(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		dir, root := beneathRoot(t)
		for _, step := range []struct {
			name              string
			create, directory bool
		}{{"a", true, true}, {"a/b", true, true}, {"a/f", true, false}, {"a/f", false, false}, {"a/b", false, true}} {
			file, err := WindowsPrivateBeneath(root, step.name, step.create, step.directory)
			if err != nil || file == nil || file.Name() != "private object" || WindowsPrivateValidateFile(file, step.directory) != nil {
				beneathClose(file)
				t.Fatal("a well-formed walk did not return a private object")
			}
			if !step.directory {
				if _, err := file.Write([]byte("x")); (err == nil) != step.create {
					beneathClose(file)
					t.Fatal("a created file must accept a write and an opened file refuse one")
				}
			}
			if file.Close() != nil {
				t.Fatal("cannot close a returned object")
			}
		}
		// Every handle is closed: a created object's DELETE access would deny
		// the guard's opens, which do not share delete.
		for _, object := range []struct {
			name      string
			directory bool
		}{{"a", true}, {`a\b`, true}, {`a\f`, false}} {
			if WindowsPrivateValidate(filepath.Join(dir, object.name), object.directory) != nil {
				t.Fatal("a created object does not carry the descriptor applied at creation")
			}
		}
	})
	t.Run("collision", func(t *testing.T) {
		for _, kind := range []string{"directory", "file", "junction", "dangling"} {
			t.Run(kind, func(t *testing.T) {
				dir, root := beneathRoot(t)
				outside, last := t.TempDir(), filepath.Join(dir, "x")
				var err error
				if kind == "file" {
					err = os.WriteFile(last, []byte("x"), 0o600)
				} else {
					err = os.Mkdir(last, 0o700)
				}
				if err != nil {
					t.Fatal("cannot plant the existing last name")
				}
				switch kind {
				case "junction":
					makeSpoolJunction(t, last, outside)
				case "dangling":
					makeSpoolJunction(t, last, filepath.Join(outside, "absent"))
				}
				before, beforeOutside := beneathTree(t, dir), beneathTree(t, outside)
				for _, directory := range []bool{true, false} {
					file, err := WindowsPrivateBeneath(root, "x", true, directory)
					beneathRefused(t, file, err)
					if !errors.Is(err, fs.ErrExist) {
						t.Fatal("an existing last name was not refused as existing")
					}
				}
				beneathUnchanged(t, before, dir)
				beneathUnchanged(t, beforeOutside, outside)
			})
		}
	})
	t.Run("component_kinds", func(t *testing.T) {
		volumeRoot, volume, drive := beneathVolume(t, t.TempDir())
		if beneathEntry(drive) != nil {
			t.Fatal("the dangling drive fixture target exists")
		}
		for _, kind := range []string{"absent", "file", "inside", "outside", "volume", "drive"} {
			t.Run(kind, func(t *testing.T) {
				dir, root := beneathRoot(t)
				outside, component := t.TempDir(), filepath.Join(dir, "a")
				targets := map[string]string{"inside": filepath.Join(dir, "inside"), "outside": outside, "volume": volume, "drive": drive}
				var err error
				switch kind {
				case "file":
					err = os.WriteFile(component, []byte("a"), 0o600)
				case "inside":
					err = errors.Join(os.Mkdir(component, 0o700), os.Mkdir(targets[kind], 0o700))
				case "outside", "volume", "drive":
					err = os.Mkdir(component, 0o700)
				}
				if err != nil {
					t.Fatal("cannot plant the intermediate component")
				}
				if target, ok := targets[kind]; ok {
					makeSpoolJunction(t, component, target)
				}
				// The volume root and the other drive are watched at the one
				// name a followed junction would create there.
				watched := []string{filepath.Join(volumeRoot, "x"), filepath.Join(drive, "x")}
				before, beforeOutside := beneathTree(t, dir), beneathTree(t, outside)
				beforeWatched := []fs.FileInfo{beneathEntry(watched[0]), beneathEntry(watched[1])}
				for _, call := range beneathCalls {
					file, err := WindowsPrivateBeneath(root, "a/x", call.create, call.directory)
					beneathRefused(t, file, err)
					// Absent is still a refusal (R5): a not-exist error is never
					// read as "absent, so proceed".
					if errors.Is(err, fs.ErrExist) || (kind == "absent" && !errors.Is(err, fs.ErrNotExist)) {
						t.Fatal("an intermediate component was refused for the wrong reason")
					}
				}
				beneathUnchanged(t, before, dir)
				beneathUnchanged(t, beforeOutside, outside)
				for index, path := range watched {
					if !beneathSameEntry(beforeWatched[index], beneathEntry(path)) {
						t.Fatal("the walk changed a junction target")
					}
				}
			})
		}
	})
	t.Run("link_kinds", func(t *testing.T) {
		for _, kind := range []string{"directory", "file"} {
			for _, last := range []bool{false, true} {
				link, name, position, calls := "a", "a/x", "intermediate", beneathCalls
				if last {
					link, name, position, calls = "x", "x", "last", beneathCalls[:2]
				}
				t.Run(kind+"_"+position, func(t *testing.T) {
					dir, root := beneathRoot(t)
					outside := t.TempDir()
					target := filepath.Join(outside, "target")
					var err error
					if kind == "file" {
						err = os.WriteFile(target, []byte("t"), 0o600)
					} else {
						err = os.Mkdir(target, 0o700)
					}
					if err != nil {
						t.Fatal("cannot create the link target")
					}
					if os.Symlink(target, filepath.Join(dir, link)) != nil {
						t.Fatal("the link rows need the symbolic-link privilege")
					}
					before, beforeOutside := beneathTree(t, dir), beneathTree(t, outside)
					for _, call := range calls {
						file, err := WindowsPrivateBeneath(root, name, call.create, call.directory)
						beneathRefused(t, file, err)
						if errors.Is(err, fs.ErrExist) != last {
							t.Fatal("a symbolic link was refused for the wrong reason")
						}
					}
					beneathUnchanged(t, before, dir)
					beneathUnchanged(t, beforeOutside, outside)
				})
			}
		}
	})
	t.Run("malformed", func(t *testing.T) {
		dir, live := beneathRoot(t)
		closed, err := os.OpenRoot(t.TempDir())
		if os.Mkdir(filepath.Join(dir, "b"), 0o700) != nil || err != nil || closed.Close() != nil {
			t.Fatal("cannot prepare the malformed name roots")
		}
		before := beneathTree(t, dir)
		for _, root := range []*os.Root{live, closed, nil} {
			for _, name := range []string{"", "/x", "x/", "a//x", "./x", "b/./x", ".", "b/../x", "b/a/..", "..", "../x", `a\b`, "a:b", "con", "a.", "a "} {
				for _, call := range beneathCalls {
					file, err := WindowsPrivateBeneath(root, name, call.create, call.directory)
					beneathRefused(t, file, err)
					if errors.Unwrap(err) != nil {
						t.Fatal("a malformed name reached a system call")
					}
				}
			}
		}
		// A closed root fails at its first system call; a nil root before any.
		for _, root := range []*os.Root{closed, nil} {
			file, err := WindowsPrivateBeneath(root, "x", true, true)
			beneathRefused(t, file, err)
			if (errors.Unwrap(err) == nil) != (root == nil) {
				t.Fatal("an unusable root was refused with the wrong cause")
			}
		}
		beneathUnchanged(t, before, dir)
	})
	t.Run("fence", func(t *testing.T) {
		for _, directory := range []bool{true, false} {
			dir, root := beneathRoot(t)
			held := filepath.Join(dir, "a")
			if os.Mkdir(held, 0o700) != nil {
				t.Fatal("cannot create the held directory")
			}
			var indexes []int
			var renamed error
			file, err := windowsPrivateBeneath(root, "a/x", true, directory, func(index int) error {
				indexes = append(indexes, index)
				renamed = os.Rename(held, filepath.Join(dir, "aside"))
				return nil
			})
			beneathClose(file)
			if !slices.Equal(indexes, []int{0}) || (!errors.Is(renamed, windows.ERROR_SHARING_VIOLATION) && !errors.Is(renamed, windows.ERROR_ACCESS_DENIED)) {
				t.Fatal("a held directory was not fenced against rename")
			}
			created := beneathEntry(filepath.Join(held, "x"))
			if err != nil || file == nil || created == nil || created.IsDir() != directory || beneathEntry(filepath.Join(dir, "aside")) != nil {
				t.Fatal("the creation did not land below the held name")
			}
		}
	})
	t.Run("convert_held", func(t *testing.T) {
		for _, directory := range []bool{true, false} {
			kind := "file"
			if directory {
				kind = "directory"
			}
			t.Run(kind, func(t *testing.T) {
				dir, root := beneathRoot(t)
				held, target := filepath.Join(dir, "a"), filepath.Join(t.TempDir(), "target")
				if os.Mkdir(held, 0o700) != nil || os.Mkdir(target, 0o700) != nil {
					t.Fatal("cannot create the conversion fixture")
				}
				buffer := spoolJunctionData(t, target)
				converted := false
				// The attribute-only writer of TestWindowsPrivateGuardRefusesInPlaceReparse:
				// no sharing mode of the held handle can deny it.
				file, err := windowsPrivateBeneath(root, "a/x", true, directory, func(int) error {
					pointer, err := windows.UTF16PtrFromString(held)
					if err != nil {
						return err
					}
					writer, err := windows.CreateFile(pointer, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
						nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
					if err != nil {
						return err
					}
					converted = setSpoolJunction(writer, buffer) == nil
					return windows.CloseHandle(writer)
				})
				if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
					beneathClose(file)
					t.Fatal("the walk created an object in the junction target")
				}
				if !converted {
					beneathClose(file)
					t.Fatal("the held directory could not be converted in place")
				}
				if file == nil {
					beneathRefused(t, file, err)
					return
				}
				var created, origin windows.ByHandleFileInformation
				anchor, openErr := root.Open(".")
				if openErr == nil {
					openErr = errors.Join(windows.GetFileInformationByHandle(windows.Handle(anchor.Fd()), &origin),
						windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &created), anchor.Close())
				}
				// Remove the object through its own handle: the converted
				// directory can then be removed as an empty junction.
				removeErr := errors.Join(windowsPrivateRollback(windows.Handle(file.Fd()), nil), file.Close())
				if openErr != nil || created.VolumeSerialNumber != origin.VolumeSerialNumber {
					t.Fatal("the walk returned an object outside the root's volume")
				}
				if removeErr != nil {
					t.Fatal("cannot remove the object created in the converted directory")
				}
			})
		}
	})
	t.Run("no_path", func(t *testing.T) {
		dir, root := beneathRoot(t)
		closed, err := os.OpenRoot(t.TempDir())
		junction := filepath.Join(dir, "j")
		if err != nil || closed.Close() != nil || os.Mkdir(filepath.Join(dir, "b"), 0o700) != nil || os.Mkdir(junction, 0o700) != nil ||
			os.WriteFile(filepath.Join(dir, "f"), []byte("f"), 0o600) != nil {
			t.Fatal("cannot prepare the refusal fixtures")
		}
		makeSpoolJunction(t, junction, t.TempDir())
		// One refusal of each kind: malformed, absent, existing, a reparse
		// component, an inherited descriptor, a closed root.
		for _, call := range []struct {
			root              *os.Root
			name              string
			create, directory bool
		}{{root, "", true, true}, {root, "absent/x", true, true}, {root, "b", true, false}, {root, "j/x", false, false}, {root, "f", false, false}, {closed, "x", true, true}} {
			file, err := WindowsPrivateBeneath(call.root, call.name, call.create, call.directory)
			beneathRefused(t, file, err)
		}
	})
}

// beneathRoot holds a new temporary directory as a root, closed before the
// directory is removed.
func beneathRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal("cannot hold the temporary directory")
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func beneathClose(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}

// beneathFailures are the texts a refusal of the walk may carry.
var beneathFailures = []error{errWindowsPrivateObject, errWindowsPrivateOwnerAdmin, errWindowsPrivateOwner, errWindowsPrivateACECount, errWindowsPrivateACEMask,
	errWindowsPrivateACEFlags, errWindowsPrivateProtected, errWindowsPrivateSystem, errWindowsPrivateSharing, errWindowsPrivateAccess, errWindowsPrivateRollback}

// beneathRefused requires a refusal that carries no path (row no_path for
// every refusal): no object, one of the private failure texts, no
// *fs.PathError, and a cause that is absent, a bare system error or the
// policy reason itself.
func beneathRefused(t *testing.T, file *os.File, err error) {
	t.Helper()
	if file != nil {
		beneathClose(file)
		t.Fatal("the walk returned an object it must refuse")
	}
	var pathErr *fs.PathError
	if err == nil || errors.As(err, &pathErr) || !slices.ContainsFunc(beneathFailures, func(reason error) bool { return reason.Error() == err.Error() }) {
		t.Fatal("a refusal is not a private failure without a path")
	}
	cause := errors.Unwrap(err)
	if _, bare := cause.(syscall.Errno); cause != nil && !bare && cause.Error() != err.Error() {
		t.Fatal("a refusal carries a cause that is not a bare system error")
	}
}

// beneathTree records every object at or below dir, never following a link,
// by relative name, each with its identity read now.
func beneathTree(t *testing.T, dir string) map[string]fs.FileInfo {
	t.Helper()
	tree := map[string]fs.FileInfo{}
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		name, relErr := filepath.Rel(dir, path)
		info := beneathEntry(path)
		if err != nil || relErr != nil || info == nil {
			return fs.ErrInvalid
		}
		tree[name] = info
		return nil
	})
	if err != nil {
		t.Fatal("cannot record a directory tree")
	}
	return tree
}

// beneathUnchanged requires the same names, kinds and identities at and below
// dir as before.
func beneathUnchanged(t *testing.T, before map[string]fs.FileInfo, dir string) {
	t.Helper()
	after := beneathTree(t, dir)
	if len(after) != len(before) {
		t.Fatal("a directory tree gained or lost an entry")
	}
	for name, info := range before {
		if !beneathSameEntry(info, after[name]) {
			t.Fatal("a directory tree entry changed")
		}
	}
}

// beneathEntry is the object at path, not followed, with its identity read now
// (os.SameFile loads and keeps it); nil when there is none.
func beneathEntry(path string) fs.FileInfo {
	info, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, info) {
		return nil
	}
	return info
}

// beneathSameEntry reports whether two entries are both absent or one object
// of one kind.
func beneathSameEntry(before, after fs.FileInfo) bool {
	if before == nil || after == nil {
		return before == nil && after == nil
	}
	return before.Mode().Type() == after.Mode().Type() && os.SameFile(before, after)
}

// beneathVolume returns the volume root holding dir, that volume's name as a
// junction targets it (Volume{...}\, read unprivileged), and a target on a
// drive letter other than the volume's.
func beneathVolume(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	path, err := windows.UTF16PtrFromString(dir)
	volumeRoot, volumeName := make([]uint16, windows.MAX_PATH), make([]uint16, windows.MAX_PATH)
	if err != nil || windows.GetVolumePathName(path, &volumeRoot[0], windows.MAX_PATH) != nil ||
		windows.GetVolumeNameForVolumeMountPoint(&volumeRoot[0], &volumeName[0], windows.MAX_PATH) != nil {
		t.Fatal("cannot read the temporary directory's volume")
	}
	rootPath, name := windows.UTF16ToString(volumeRoot), windows.UTF16ToString(volumeName)
	if len(rootPath) < 3 || !strings.HasPrefix(name, `\\?\Volume{`) {
		t.Fatal("the temporary directory's volume has no volume name")
	}
	drive := "Z"
	if strings.EqualFold(rootPath[:1], drive) {
		drive = "Y"
	}
	return rootPath, strings.TrimPrefix(name, `\\?\`), drive + `:\beneath-absent`
}
