//go:build windows

package runtimeconfig

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_RENAME_INFO has pointer-sized alignment before RootDirectory. The
// FileRenameInfoEx variant interprets the first DWORD as Flags, not a BOOLEAN.
type windowsPrivateRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// MoveFileEx's legacy replacement refuses an open target even when all readers
// grant delete sharing. FileRenameInfoEx POSIX semantics replace the name while
// those readers retain the old object. WindowsPrivateRename separately probes
// target DELETE access to preserve the sharing-denied refusal. File data was
// already synced before publication; this does not claim directory durability.
// https://learn.microsoft.com/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information
func windowsPrivateReplace(handle windows.Handle, path string) error {
	name, err := windows.UTF16FromString(path)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	var layout windowsPrivateRenameInfo
	offset := int(unsafe.Offsetof(layout.FileName))
	buffer := make([]byte, max(int(unsafe.Sizeof(layout)), offset+len(name)*2))
	info := (*windowsPrivateRenameInfo)(unsafe.Pointer(&buffer[0]))
	const replaceIfExists, posixSemantics = 1, 2
	info.Flags = replaceIfExists | posixSemantics
	info.FileNameLength = uint32((len(name) - 1) * 2)
	for index, character := range name {
		binary.LittleEndian.PutUint16(buffer[offset+index*2:], character)
	}
	return windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer)))
}
