//go:build windows

package localcell

import (
	"errors"
	"os"
	"strings"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// drillPlatformMkdir makes a private directory through a held root by
// runtimeconfig's handle-relative walk, which never resolves a pathname or
// follows a reparse point: the drill directory, exactly one plain component, in
// recovery/ when drill is nil, else name inside the drill. The private DACL is
// applied at creation; an error carries no path. The created directory's handle
// keeps delete access, which would refuse a walk through it: it is closed here.
func drillPlatformMkdir(_ *Cell, boundary, drill *os.Root, drillID, name string) error {
	root := drill
	if drill == nil {
		if strings.Contains(drillID, "/") {
			return windows.ERROR_INVALID_NAME
		}
		root, name = boundary, drillID
	}
	made, err := runtimeconfig.WindowsPrivateBeneath(root, name, true, true)
	if err == nil {
		err = made.Close()
	}
	return err
}

// drillPlatformCreate creates a private file exclusively through the held drill
// root; the file's name is fixed, so no later error of it carries a path. The
// writer keeps delete access: while it is open, a plain open of the file fails.
func drillPlatformCreate(_ *Cell, _, drill *os.Root, _, name string) (*os.File, error) {
	return runtimeconfig.WindowsPrivateBeneath(drill, name, true, false)
}

// drillPlatformOpen opens an existing private directory or file for reading
// through the same walk; a reparse point at any component is refused; a
// directory's handle can neither list nor sync.
func drillPlatformOpen(root *os.Root, name string, directory bool) (*os.File, error) {
	return runtimeconfig.WindowsPrivateBeneath(root, name, false, directory)
}

// drillPlatformAllocate sets the end of file to offset+length: on a file that
// is neither sparse nor compressed NTFS allocates the clusters to the file, and
// the allocation outlives the handle (allocation information alone does not).
func drillPlatformAllocate(f *os.File, offset, length uint64) error {
	return windows.Ftruncate(windows.Handle(f.Fd()), int64(offset+length))
}

// drillPlatformRefusal: a full disk, a quota or a size limit is storage_exhausted.
func drillPlatformRefusal(err error) string {
	switch {
	case errors.Is(err, windows.ERROR_DISK_FULL), errors.Is(err, windows.ERROR_HANDLE_DISK_FULL),
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED), errors.Is(err, windows.ERROR_FILE_TOO_LARGE):
		return "storage_exhausted"
	}
	return ""
}

// drillPlatformVerify proves [0, size) of f allocated by the file system: it
// reads FileStandardInfo (FILE_STANDARD_INFO, winbase.h) and the attributes by
// handle, and drillWindowsProved decides; no claim about the device below.
func drillPlatformVerify(f *os.File, size uint64) error {
	handle := windows.Handle(f.Fd())
	var standard struct {
		allocation, end          int64
		links                    uint32
		deletePending, directory bool
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard))) != nil ||
		windows.GetFileInformationByHandle(handle, &info) != nil || !drillWindowsProved(standard.allocation, standard.end, info.FileAttributes, size) {
		return drillStorageError{reason: "allocation_unproved"}
	}
	return nil
}
