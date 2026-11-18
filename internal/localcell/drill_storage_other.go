//go:build !linux && !darwin && !windows

package localcell

import "os"

// drillPlatformVolume refuses every volume outside the supported table.
func drillPlatformVolume(*os.File) (drillVolume, error) {
	return drillVolume{}, drillStorageError{reason: "unsupported_fs"}
}
