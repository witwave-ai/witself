package main

import (
	"os"
	"runtime"
)

// Unix permission bits are authoritative on macOS and Linux. Native Windows
// secures these files with ACLs and exposes only a lossy read-only projection
// through os.FileMode, so checks such as 0600 and 0700 cannot be compared
// literally there. Structural, reparse-point, file-identity, and content
// checks remain mandatory on every platform.
func integrationFileModeMatches(mode, expected os.FileMode) bool {
	return integrationFileModeMatchesPlatform(runtime.GOOS, mode, expected)
}

func integrationFileModeMatchesPlatform(platform string, mode, expected os.FileMode) bool {
	return platform == "windows" || mode.Perm() == expected.Perm()
}

func integrationExecutableModeIsUsable(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() &&
		(runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

func integrationFileModeFingerprint(mode os.FileMode) uint32 {
	if runtime.GOOS == "windows" {
		// Persist the requested restricted mode instead of Go's lossy Windows
		// projection so crash-recovery fingerprints remain stable.
		return 0o600
	}
	return uint32(mode.Perm())
}
