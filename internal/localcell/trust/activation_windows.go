package trust

import (
	"context"
	"os"
	"syscall"
)

// Windows persistent privacy requires an ACL adapter, deliberately absent in B3.
func privateOwned(_ os.FileInfo) bool                  { return false }
func openHostExecutable(name string) (*os.File, error) { return openArchiveFile(name) }
func hostFileIdentity(file *os.File, _ os.FileInfo) (HostIdentity, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return HostIdentity{}, ReasonPayloadIdentity
	}
	return HostIdentity{Device: uint64(info.VolumeSerialNumber), Inode: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, nil
}
func generationProcessReferenced(context.Context, string) (bool, error) {
	return false, ReasonProcessObservationUnavailable
}
