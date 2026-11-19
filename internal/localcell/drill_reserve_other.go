//go:build !linux && !windows

package localcell

import "os"

// Outside the supported table admission refuses before anything is written;
// these refuse again and write nothing.
func drillPlatformMkdir(*Cell, *os.Root, *os.Root, string, string) error {
	return drillStorageError{reason: "unsupported_fs"}
}
func drillPlatformCreate(*Cell, *os.Root, *os.Root, string, string) (*os.File, error) {
	return nil, drillStorageError{reason: "unsupported_fs"}
}
func drillPlatformOpen(*os.Root, string, bool) (*os.File, error) {
	return nil, drillStorageError{reason: "unsupported_fs"}
}
func drillPlatformAllocate(*os.File, uint64, uint64) error {
	return drillStorageError{reason: "unsupported_fs"}
}
func drillPlatformVerify(*os.File, uint64) error { return drillStorageError{reason: "unsupported_fs"} }
func drillPlatformRefusal(error) string          { return "" }
